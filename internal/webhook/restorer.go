// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Antoni Romera

package webhook

import (
	"context"
	"fmt"
	"net/http"

	"gomodules.xyz/jsonpatch/v2"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	doblurav1alpha1 "github.com/doblura/doblura/api/v1alpha1"
)

// RestorerPath is where the restore-stamping webhook is served.
const RestorerPath = "/mutate-doblura-dev-v1alpha1-odoorestore"

// +kubebuilder:webhook:path=/mutate-doblura-dev-v1alpha1-odoorestore,mutating=true,failurePolicy=Fail,matchPolicy=Equivalent,sideEffects=None,timeoutSeconds=5,groups=doblura.dev,resources=odoorestores,verbs=create,versions=v1alpha1,name=restorer.odoorestore.doblura.dev,admissionReviewVersions=v1,serviceName=doblura-webhook,serviceNamespace=doblura-system

// RestoreStamp records who asked for a restore.
//
// The same reasoning as the environment's creator stamp, and more force behind
// it. A restore replaces a customer's database; the object exists to be the
// record of that, and a record with no name on it is not one.
//
// Taken from the authenticated identity and never from the object, for the
// reason that always applies to self-reported identity: a client that can write
// its own creator can write somebody else's.
//
// failurePolicy is Fail. If this can be skipped, a restore can exist with no
// name attached — and the case where somebody wants that is exactly the case
// where the name matters.
// It also decides whether the current contents are copied before being replaced,
// which is the one guardrail on this object with teeth. See spec.safetyCopy for
// why a copy rather than another string to type; here it is enough to say that
// the decision depends on the TARGET, which is why it cannot be a schema default
// and cannot be left to the person filling in the form.
type RestoreStamp struct {
	Decoder admission.Decoder
	// Reader reads the target environment and whatever backs it up. Without it
	// this webhook could only stamp a name, and the safety copy would have to be
	// a field somebody remembers to set on the day they are least calm.
	//
	// The API reader and NOT the cache. This was the cache first, on the reasoning
	// that a restore does not race an environment created seconds ago — which was
	// wrong about the BACKUP: create one in the console and restore immediately,
	// and the cache has not seen it yet. The observed result was a Production
	// restore refused for having no backup that plainly existed, and a Staging one
	// quietly getting no safety copy at all.
	Reader client.Reader
}

func (m *RestoreStamp) Handle(ctx context.Context, req admission.Request) admission.Response {
	var rs doblurav1alpha1.OdooRestore
	if err := m.Decoder.Decode(req, &rs); err != nil {
		return admission.Errored(http.StatusBadRequest, err)
	}

	// An annotation and not the status: status is written by the controller and
	// a mutating webhook cannot set it on create. The controller copies it
	// across, so the field somebody reads is on the object where they look.
	patch := jsonpatch.NewOperation("add",
		"/metadata/annotations/"+escapeJSONPointer(RequestedByAnnotation), req.UserInfo.Username)
	if rs.Annotations == nil {
		patch = jsonpatch.NewOperation("add", "/metadata/annotations",
			map[string]string{RequestedByAnnotation: req.UserInfo.Username})
	}
	patches := []jsonpatch.JsonPatchOperation{patch}

	safety, reason, err := m.resolveSafetyCopy(ctx, &rs)
	if err != nil {
		return admission.Denied(err.Error())
	}
	// Written even when it resolves to the same value the client sent. The field
	// is what the controller reads and what somebody looks at afterwards to see
	// whether there was a way back, and "unset" is not an answer to that.
	patches = append(patches, jsonpatch.NewOperation("add", "/spec/safetyCopy", safety))

	return admission.Patched(reason, patches...)
}

// resolveSafetyCopy decides whether what is in the target gets copied first.
//
// The rules, in the order they are applied:
//
//   - Production is copied first, always, and cannot be turned off. Somebody who
//     genuinely wants to replace a production database with no way back can delete
//     the copy afterwards; there is no reason to make it possible in one step.
//   - Production with nothing backing it up is refused outright. Not because the
//     restore would fail — it would work — but because a production database that
//     nothing is copying should not first be discovered on the day somebody
//     replaces it.
//   - Anything else persistent defaults to copying, and can be turned off.
//   - An ephemeral environment defaults to not copying. Its data is a review
//     copy nobody will miss, and a mandatory copy would only teach people that
//     the setting is noise.
func (m *RestoreStamp) resolveSafetyCopy(
	ctx context.Context,
	rs *doblurav1alpha1.OdooRestore,
) (bool, string, error) {
	var env doblurav1alpha1.OdooEnvironment
	err := m.Reader.Get(ctx, client.ObjectKey{
		Namespace: rs.Namespace, Name: rs.Spec.Into,
	}, &env)
	switch {
	case apierrors.IsNotFound(err):
		// Refused here rather than left to the controller. The controller would
		// also refuse, but only after this object existed and somebody had been
		// told the restore was under way — and the overwhelmingly likely cause is
		// a typo in the one field that says whose data gets replaced.
		return false, "", fmt.Errorf(
			"there is no environment called %q in %s, so there is nothing to "+
				"restore into. Check the name: it is the field that decides "+
				"whose database is replaced", rs.Spec.Into, rs.Namespace)
	case err != nil:
		return false, "", fmt.Errorf("reading the environment being restored into: %w", err)
	}

	production := env.Spec.Purpose == doblurav1alpha1.PurposeProduction
	persistent := production ||
		env.Spec.Lifecycle.Type == doblurav1alpha1.LifecyclePersistent

	asked := rs.Spec.SafetyCopy
	if production && asked != nil && !*asked {
		return false, "", fmt.Errorf(
			"%s is a Production environment, so a copy of what is in it now is "+
				"taken before it is replaced, and that cannot be turned off. "+
				"Remove safetyCopy: false. The copy is what makes this restore "+
				"undoable, and a restore from the wrong copy is the mistake no "+
				"acknowledgement text can catch", env.Name)
	}

	want := persistent
	if asked != nil {
		want = *asked
	}
	if !want {
		return false, fmt.Sprintf(
			"recorded who asked; no copy is taken of %s first", env.Name), nil
	}

	// Somewhere to write it. Asked now rather than in the Job, because "the
	// restore stopped halfway because there was nowhere to put the safety copy"
	// is a sentence nobody should read while an environment is switched off.
	holder, err := m.backupHolding(ctx, rs.Namespace, env.Name)
	switch {
	case err != nil:
		return false, "", err
	case holder == "":
		if production {
			return false, "", fmt.Errorf(
				"%s is a Production environment and no OdooBackup is copying it, "+
					"so there is nowhere to put a copy of what is in it now — and "+
					"this restore will not replace a production database that has "+
					"nothing backing it up. Create an OdooBackup for %s first. "+
					"That is worth doing whether or not you go on to restore",
				env.Name, env.Name)
		}
		// Refused, whether it was asked for or defaulted. The first version let a
		// defaulted copy fall back to "no copy" here, on the grounds that doblura
		// does not invent a volume — and that fallback is invisible: an admission
		// response on an ALLOWED request is not shown to whoever made it, so the
		// person would be told nothing and get no copy.
		//
		// A refusal is the only version of this that reaches a human. It also
		// leaves them both answers: create a backup, or say safetyCopy: false and
		// mean it.
		return false, "", fmt.Errorf(
			"a copy of %s is taken before it is replaced, but no OdooBackup with a "+
				"volume is copying that environment, so there is nowhere to write "+
				"it. Either create an OdooBackup for %s — worth doing whether or "+
				"not you restore — or set safetyCopy: false and accept that this "+
				"restore cannot be undone", env.Name, env.Name)
	}

	return true, fmt.Sprintf(
		"recorded who asked; %s is copied into %s before being replaced",
		env.Name, holder), nil
}

// backupHolding is the OdooBackup that copies this environment, if any.
//
// The first one, by name, when there are several. Several backups of one
// environment is a legitimate setup — a frequent local one and a slow offsite one
// — and for this purpose any of them is somewhere the copy can go.
func (m *RestoreStamp) backupHolding(ctx context.Context, ns, env string) (string, error) {
	var list doblurav1alpha1.OdooBackupList
	if err := m.Reader.List(ctx, &list, client.InNamespace(ns)); err != nil {
		return "", fmt.Errorf("looking for a backup of %s: %w", env, err)
	}
	best := ""
	for i := range list.Items {
		b := &list.Items[i]
		if b.Spec.Environment != env || b.Spec.Destination.Volume == nil {
			continue
		}
		if best == "" || b.Name < best {
			best = b.Name
		}
	}
	return best, nil
}

// RequestedByAnnotation carries the authenticated identity of whoever asked.
const RequestedByAnnotation = "doblura.dev/requested-by"
