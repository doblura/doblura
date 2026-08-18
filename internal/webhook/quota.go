// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Antoni Romera

// Package webhook holds Doblura's admission webhooks and the certificate
// plumbing they need.
package webhook

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"strings"

	"gomodules.xyz/jsonpatch/v2"
	"k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	doblurav1alpha1 "github.com/doblura/doblura/api/v1alpha1"
)

// ─────────────── The quota, enforced where it can still be refused ───────────────
//
// `OdooTenant.spec.maxEphemeralEnvironments` shipped as a field that nothing read.
// That is worse than not having it: the number is on the customer record, an
// operator reads it and believes the cluster is bounded, and the first person to
// open an environment per ticket finds out on a Friday that it never was.
//
// Why a webhook and not the reconciler
// ────────────────────────────────────
// The reconciler runs after the object exists. Refusing there means fifty
// OdooEnvironment objects, fifty databases already created by whoever got there
// first, and a `status.message` on each explaining that it should not have
// happened. A quota has to be a rejection at apply time or it is a cleanup task.
//
// Why not CEL, which is where every other Doblura guardrail lives
// ──────────────────────────────────────────────────────────────
// Same reason as the handover check: answering the question means reading OTHER
// objects — the tenant, and every environment already open — and CEL validation
// only ever sees the object being admitted. This is the case the CRD cannot do.
//
// Two limits, and they close different holes
// ──────────────────────────────────────────
//  1. Per tenant, from the customer record. Stops one demanding customer from
//     starving the others.
//  2. Per person, cluster-wide, from operator configuration. Stops the case that
//     actually exhausts a cluster: fifty environments spread over twenty
//     customers passes every per-tenant limit, and an environment with no
//     `forTenant` at all passes them without even being counted.
//
// The second one is deliberately NOT per namespace. The persona ClusterRoles are
// meant to be bound cluster-wide (that is the documented one-liner in the README),
// so an allowance that resets per namespace is an allowance multiplied by the
// number of namespaces in the cluster.
//
// Whose environment it is, and why that is not the client's word
// ─────────────────────────────────────────────────────────────
// A per-person count needs an owner recorded on each object, and an object cannot
// be trusted to name its own creator: anyone under a quota could write somebody
// else's name and get a fresh allowance. So the creator comes from the
// AdmissionRequest's UserInfo — which the API server fills in from the
// authenticated identity and no client can influence — and it is stamped by the
// mutating webhook in this package. The validating half then refuses any object
// whose annotation does not match the caller, which is what makes the count worth
// doing arithmetic on.

const (
	// CreatorAnnotation records who asked for the environment.
	//
	// An annotation and not a label on purpose: usernames from an OIDC provider
	// are e-mail addresses, and label VALUES are restricted to a DNS-ish
	// character set that `@` is not in. The cost is that the count cannot be a
	// label selector and has to filter in Go.
	CreatorAnnotation = "doblura.dev/created-by"

	// MutatePath and ValidatePath are where the two halves are served. They are
	// also written into the generated manifests by the markers below, so they
	// only agree because both come from these constants — a mismatch would mean
	// a 404 from the webhook server, and with failurePolicy Fail that is every
	// create refused.
	MutatePath   = "/mutate-doblura-dev-v1alpha1-odooenvironment"
	ValidatePath = "/validate-doblura-dev-v1alpha1-odooenvironment"
)

// ─────────────── failurePolicy: Fail, and the reasoning ───────────────
//
// This is the decision most likely to be reversed by somebody who has just been
// paged, so the argument is written down rather than left in a marker.
//
// Ignore turns an outage into a silent bypass. If the webhook is unreachable the
// API server admits the create, the quota stops applying, and NOTHING SAYS SO —
// which is the exact failure this whole change exists to fix. A limit that
// evaporates when a pod restarts is not a limit, it is a comment.
//
// Fail's blast radius is small enough to state completely:
//
//   - The rules match `doblura.dev/odooenvironments` on CREATE only. Nothing else
//     in the cluster is affected — not other kinds, not other verbs.
//   - UPDATE and DELETE are not intercepted, so during an outage existing
//     environments keep reconciling, keep serving, and can still be DELETED.
//     That last one matters: the way out of a full quota stays open while the
//     thing that enforces it is down.
//   - The webhook is served by the operator itself. "The webhook is down" means
//     "the operator is down", and an OdooEnvironment admitted while the operator
//     is down would sit in Pending anyway. Fail costs a clear rejection instead
//     of an object that does nothing.
//   - No controller creates OdooEnvironment objects today, and the one that will
//     (the ReviewSet-style flow) runs as the operator's own ServiceAccount, which
//     is exempt below — so a human's limit can never throttle the system.
//
// timeoutSeconds is 5 rather than the default 10: a hung webhook should fail
// fast. The whole handler is two reads against the API server.
//
// +kubebuilder:webhookconfiguration:mutating=false,name=doblura-quota
// +kubebuilder:webhookconfiguration:mutating=true,name=doblura-creator

// The markers live in their own comment block, separated from the doc comment
// below: controller-gen only collects package-scoped markers from a free-floating
// comment, and one attached to the type is silently ignored — no manifest, no
// warning.
//
// +kubebuilder:webhook:path=/mutate-doblura-dev-v1alpha1-odooenvironment,mutating=true,failurePolicy=Fail,matchPolicy=Equivalent,sideEffects=None,timeoutSeconds=5,groups=doblura.dev,resources=odooenvironments,verbs=create,versions=v1alpha1,name=creator.odooenvironment.doblura.dev,admissionReviewVersions=v1,serviceName=doblura-webhook,serviceNamespace=doblura-system

// EnvironmentCreator is the mutating half: it stamps who is asking.
type EnvironmentCreator struct {
	Decoder admission.Decoder

	// Client reads the customer record, for the environment defaults. Nil is a
	// supported configuration: the creator stamp still works, and environments
	// simply have to carry their own infrastructure fields.
	Client client.Client
}

// Handle writes the caller's identity into the creator annotation, replacing
// whatever the client sent.
//
// failurePolicy is Fail here too, and for a reason that is not obvious: if this
// half can be skipped, an OdooEnvironment can exist without a creator, and every
// object without a creator is one that the per-person count cannot see. A
// stamping webhook that is allowed to fail open produces exactly the invisible
// under-counting the quota is supposed to prevent.
func (m *EnvironmentCreator) Handle(ctx context.Context, req admission.Request) admission.Response {
	var env doblurav1alpha1.OdooEnvironment
	if err := m.Decoder.Decode(req, &env); err != nil {
		return admission.Errored(http.StatusBadRequest, err)
	}

	// Both, from the client, is a mistake worth naming. Checked here rather than
	// in CEL because CEL runs after this webhook, by which point resolving the
	// catalogue name has legitimately set both.
	if env.Spec.ImageRef != "" && env.Spec.Image != "" {
		return admission.Denied(
			"name either spec.imageRef or spec.image, not both: a precedence rule is one more " +
				"thing to remember, and remembering it wrong here means running a different " +
				"version of the product than the screen says")
	}

	// A hand-built patch rather than decode-mutate-remarshal.
	//
	// The usual PatchResponseFromRaw round-trip diffs the whole object, so it
	// also carries every difference between what the client sent and what our Go
	// types produce. For a security control that is the wrong shape: this patch
	// touches one annotation and provably nothing else.
	patch := jsonpatch.NewOperation("add", "/metadata/annotations/"+escapeJSONPointer(CreatorAnnotation), req.UserInfo.Username)
	if env.Annotations == nil {
		// RFC 6902 refuses to add a member to an object that does not exist, so
		// the whole map has to be created in one operation.
		patch = jsonpatch.NewOperation("add", "/metadata/annotations",
			map[string]string{CreatorAnnotation: req.UserInfo.Username})
	}
	ops, deny := m.defaultsFrom(ctx, &env)
	if deny != "" {
		return admission.Denied(deny)
	}
	return admission.Patched("recorded the creator from the authenticated identity",
		append([]jsonpatch.Operation{patch}, ops...)...)
}

// defaultsFrom fills the infrastructure fields from the customer record.
//
// Done here rather than in the console, and that is the load-bearing choice. The
// console impersonates the person and holds no privilege of its own, so anything
// it can do, `kubectl` can do — if defaulting lived there, an environment created
// from the command line would be missing exactly the four fields the person
// creating it does not know, and the interface would have quietly become a
// privileged path.
//
// It fills only fields left entirely empty, and never merges half an object. An
// environment naming a database host but no user is a mistake, and completing it
// from the customer's credentials would produce a working connection to a server
// nobody asked for.
func (m *EnvironmentCreator) defaultsFrom(
	ctx context.Context,
	env *doblurav1alpha1.OdooEnvironment,
) ([]jsonpatch.Operation, string) {
	if m.Client == nil || env.Spec.ForTenant == "" {
		return nil, ""
	}
	var tenant doblurav1alpha1.OdooTenant
	if err := m.Client.Get(ctx, client.ObjectKey{
		Namespace: env.Namespace, Name: env.Spec.ForTenant,
	}, &tenant); err != nil {
		// Deliberately not an error. A missing customer record is caught by the
		// validating half with a message about the tenant; failing here would
		// report it as a defaulting failure, which names the wrong thing.
		return nil, ""
	}

	var ops []jsonpatch.Operation

	// An imageRef that does not resolve is REFUSED, never quietly replaced by the
	// customer's default. Falling back was the first implementation and it was
	// wrong in the worst available way: a typo in a catalogue name produced a
	// working environment running a different version of the product than the
	// person asked for, with nothing anywhere saying so.
	if env.Spec.ImageRef != "" {
		e := tenant.Spec.ImageByName(env.Spec.ImageRef)
		if e == nil {
			return nil, fmt.Sprintf(
				"%q is not in %s's image catalogue. Available: %s",
				env.Spec.ImageRef, tenant.Name, catalogueNames(&tenant))
		}
		return append(ops, jsonpatch.NewOperation("add", "/spec/image", e.Image)), ""
	}

	d := tenant.Spec.EnvironmentDefaults
	if d == nil {
		// No environmentDefaults, but a catalogue may still answer the question.
		if e := tenant.Spec.DefaultImage(); e != nil && env.Spec.Image == "" {
			ops = append(ops, jsonpatch.NewOperation("add", "/spec/image", e.Image))
		}
		return ops, ""
	}

	if env.Spec.Image == "" {
		// The catalogue's default wins over environmentDefaults.image: the
		// catalogue is the field a person edits when they change versions, and
		// the other is a leftover from before there was one.
		if e := tenant.Spec.DefaultImage(); e != nil {
			ops = append(ops, jsonpatch.NewOperation("add", "/spec/image", e.Image))
		} else if d.Image != "" {
			ops = append(ops, jsonpatch.NewOperation("add", "/spec/image", d.Image))
		}
	}
	if env.Spec.Database.Host == "" && d.Database != nil {
		ops = append(ops, jsonpatch.NewOperation("add", "/spec/database", d.Database))
	}
	if (env.Spec.Storage == nil || env.Spec.Storage.Filestore == nil) && d.Storage != nil {
		ops = append(ops, jsonpatch.NewOperation("add", "/spec/storage", d.Storage))
	}
	if env.Spec.Size == "" && d.Size != "" {
		ops = append(ops, jsonpatch.NewOperation("add", "/spec/size", d.Size))
	}
	return ops, ""
}

// catalogueNames lists what the person could have meant.
//
// Printed in the refusal because "hms-18 is not in the catalogue" leaves them
// guessing, and the list is three or four short names.
func catalogueNames(t *doblurav1alpha1.OdooTenant) string {
	if len(t.Spec.Images) == 0 {
		return "the catalogue is empty"
	}
	names := make([]string, 0, len(t.Spec.Images))
	for i := range t.Spec.Images {
		names = append(names, t.Spec.Images[i].Name)
	}
	sort.Strings(names)
	return strings.Join(names, ", ")
}


// escapeJSONPointer encodes a map key for use in a JSON Pointer path. The
// annotation contains a `/`, which is the pointer's own separator.
func escapeJSONPointer(s string) string {
	return strings.ReplaceAll(strings.ReplaceAll(s, "~", "~0"), "/", "~1")
}

// +kubebuilder:webhook:path=/validate-doblura-dev-v1alpha1-odooenvironment,mutating=false,failurePolicy=Fail,matchPolicy=Equivalent,sideEffects=None,timeoutSeconds=5,groups=doblura.dev,resources=odooenvironments,verbs=create,versions=v1alpha1,name=quota.odooenvironment.doblura.dev,admissionReviewVersions=v1,serviceName=doblura-webhook,serviceNamespace=doblura-system

// EnvironmentQuota is the validating half: it counts, and refuses.
type EnvironmentQuota struct {
	// Reader goes STRAIGHT TO THE API SERVER, not through the manager's cache.
	//
	// The cache lags by however long the informer takes to see a create, and a
	// quota that a shell loop beats is not one. Two live reads on a
	// human-initiated create is a price worth paying; this is not a hot path.
	Reader  client.Reader
	Decoder admission.Decoder

	// ExemptUsers are the identities the quota does not apply to: the operator's
	// own ServiceAccount, which creates environments on behalf of the cluster and
	// must never be throttled by a person's allowance.
	//
	// Named identities rather than "any ServiceAccount" or a group. Exempting a
	// group would mean the bypass is whatever anyone with RBAC on RoleBindings
	// decides it is, and that is not a quota either.
	ExemptUsers map[string]bool

	// MaxPerCreator is how many ephemeral environments one person may hold at
	// once, across every customer and every namespace. Operator configuration
	// rather than a CRD field: it is a property of the installation, not of any
	// one customer, and there is nowhere on a per-customer record for it to live
	// without being wrong.
	MaxPerCreator int32
}

// MaxPerCreatorDefault is the per-person allowance when the operator was not told
// one. Deliberately larger than a tenant's default of 3: one person legitimately
// works on more than one customer at a time, and the point of this limit is to
// catch runaway self-service, not to make the tool annoying.
const MaxPerCreatorDefault int32 = 5

func (q *EnvironmentQuota) Handle(ctx context.Context, req admission.Request) admission.Response {
	l := log.FromContext(ctx)

	if q.ExemptUsers[req.UserInfo.Username] {
		return admission.Allowed("exempt identity: the operator creates environments on the cluster's behalf and is not subject to a person's quota")
	}

	var env doblurav1alpha1.OdooEnvironment
	if err := q.Decoder.Decode(req, &env); err != nil {
		return admission.Errored(http.StatusBadRequest, err)
	}

	// Only ephemeral environments are counted. A Persistent one is somebody's
	// staging or production: it went through a decision, it is not throwaway, and
	// capping it with a number meant for scratch copies would break the tool for
	// the case it is most needed in.
	if !isEphemeral(&env) {
		return admission.Allowed(fmt.Sprintf(
			"lifecycle.type is %s: the quota only counts Ephemeral environments", env.Spec.Lifecycle.Type))
	}

	// The annotation must be the one the mutating half stamped. If it is not, the
	// count is being fed a name the caller chose, and every number below it is
	// fiction.
	//
	// This can only fire when the mutating webhook was not consulted — removed,
	// misrouted, or its rules edited — so the message names that rather than
	// blaming the caller, who cannot do anything about it.
	if got := env.Annotations[CreatorAnnotation]; got != req.UserInfo.Username {
		return admission.Denied(fmt.Sprintf(
			"%s is set by the admission webhook from the authenticated identity, not by the client: "+
				"this object says %q and the request came from %q. "+
				"That normally means the MutatingWebhookConfiguration doblura-creator is missing or does not match "+
				"odooenvironments — ask whoever administers the Doblura release to check it, because until it is "+
				"fixed the per-person quota cannot be counted",
			CreatorAnnotation, got, req.UserInfo.Username))
	}

	if resp, done := q.checkPerCreator(ctx, req); done {
		return resp
	}
	if resp, done := q.checkPerTenant(ctx, req, &env); done {
		return resp
	}

	l.V(1).Info("environment admitted", "creator", req.UserInfo.Username, "tenant", env.Spec.ForTenant)
	return admission.Allowed("within the ephemeral-environment quota")
}

// checkPerCreator applies the per-person, cluster-wide allowance.
func (q *EnvironmentQuota) checkPerCreator(
	ctx context.Context,
	req admission.Request,
) (admission.Response, bool) {
	limit := q.MaxPerCreator
	if limit <= 0 {
		// Zero means "not configured", not "nobody may create anything". A
		// default of zero on an operator flag would be a fail-closed cluster on
		// upgrade, which is not a decision anyone made.
		limit = MaxPerCreatorDefault
	}

	var all doblurav1alpha1.OdooEnvironmentList
	if err := q.Reader.List(ctx, &all); err != nil {
		// Fail closed. An error here is the same situation as the webhook being
		// unreachable, and admitting on a failed read would make "make the API
		// server slow" a way through the quota.
		return admission.Errored(http.StatusInternalServerError,
			fmt.Errorf("could not count the environments already open: %w", err)), true
	}

	mine := open(all.Items, func(e *doblurav1alpha1.OdooEnvironment) bool {
		return e.Annotations[CreatorAnnotation] == req.UserInfo.Username
	})
	if int32(len(mine)) < limit {
		return admission.Response{}, false
	}

	return admission.Denied(fmt.Sprintf(
		"you already hold %d of your %d ephemeral environments: %s. "+
			"Delete one you have finished with (kubectl delete odooenvironment <name> -n <namespace>), "+
			"or ask whoever administers the Doblura release to raise webhook.maxEnvironmentsPerCreator",
		len(mine), limit, describe(mine))), true
}

// checkPerTenant applies the limit on the customer record.
func (q *EnvironmentQuota) checkPerTenant(
	ctx context.Context,
	req admission.Request,
	env *doblurav1alpha1.OdooEnvironment,
) (admission.Response, bool) {
	// No customer declared: an internal environment, and there is no customer
	// record to read a limit from. It is not unbounded — the per-person limit
	// above already counted it, which is why that one is not optional.
	if env.Spec.ForTenant == "" {
		return admission.Response{}, false
	}

	var tenant doblurav1alpha1.OdooTenant
	err := q.Reader.Get(ctx, client.ObjectKey{Namespace: env.Namespace, Name: env.Spec.ForTenant}, &tenant)
	switch {
	case errors.IsNotFound(err):
		// The same decision as the handover check makes for an unknown database:
		// Doblura stays usable before the catalogue is filled in. There is no
		// limit to read, the per-person limit still bounds the damage, and
		// refusing here would teach people to stop declaring forTenant — which
		// would cost the handover guardrail too.
		log.FromContext(ctx).Info("no customer record for the declared tenant; only the per-person quota applies",
			"tenant", env.Spec.ForTenant, "namespace", env.Namespace)
		return admission.Response{}, false
	case err != nil:
		return admission.Errored(http.StatusInternalServerError,
			fmt.Errorf("could not read OdooTenant %q: %w", env.Spec.ForTenant, err)), true
	}

	limit := tenant.Spec.EphemeralQuota()

	// A quota of zero is a decision, not a full cupboard, and it deserves its own
	// sentence: telling somebody to delete an environment to make room would send
	// them looking for one that does not exist.
	if limit == 0 {
		return admission.Denied(fmt.Sprintf(
			"customer %q is set to zero ephemeral environments (spec.maxEphemeralEnvironments on OdooTenant/%s -n %s), "+
				"so no throwaway copy of their data may be opened at all. If that is out of date, "+
				"someone with the doblura-platform profile can raise it",
			env.Spec.ForTenant, env.Spec.ForTenant, env.Namespace)), true
	}

	var inNamespace doblurav1alpha1.OdooEnvironmentList
	if err := q.Reader.List(ctx, &inNamespace, client.InNamespace(env.Namespace)); err != nil {
		return admission.Errored(http.StatusInternalServerError,
			fmt.Errorf("could not count the environments already open for %s: %w", env.Spec.ForTenant, err)), true
	}
	theirs := open(inNamespace.Items, func(e *doblurav1alpha1.OdooEnvironment) bool {
		return e.Spec.ForTenant == env.Spec.ForTenant
	})
	if int32(len(theirs)) < limit {
		return admission.Response{}, false
	}

	// The refusal has to end in a next step. Somebody is reading this in a
	// terminal at the moment they wanted to help a customer, and "quota exceeded"
	// leaves them with nothing to do but complain about the tool.
	mine := open(inNamespace.Items, func(e *doblurav1alpha1.OdooEnvironment) bool {
		return e.Spec.ForTenant == env.Spec.ForTenant &&
			e.Annotations[CreatorAnnotation] == req.UserInfo.Username
	})
	return admission.Denied(fmt.Sprintf(
		"customer %q is at its ephemeral-environment quota: %d of %d open (%d of them yours): %s. "+
			"Delete one that is no longer needed (kubectl delete odooenvironment <name> -n %s), "+
			"or ask someone with the doblura-platform profile to raise spec.maxEphemeralEnvironments "+
			"on OdooTenant/%s -n %s",
		env.Spec.ForTenant, len(theirs), limit, len(mine), describe(theirs),
		env.Namespace, env.Spec.ForTenant, env.Namespace)), true
}

// open lists the matching environments that still hold resources.
//
// What counts is "does this environment still own a database and a Deployment",
// not "is it healthy". So a Failed one COUNTS: it has a database, it is occupying
// the customer's allowance, and quietly not counting it would let a person
// accumulate broken environments without limit — while also hiding the pile from
// the person best placed to clear it.
//
// What does not count is an environment on its way out: a DeletionTimestamp means
// the finalizer is dropping the database right now, and Expired means the
// controller has already asked for the deletion.
func open(items []doblurav1alpha1.OdooEnvironment, match func(*doblurav1alpha1.OdooEnvironment) bool) []string {
	var out []string
	for i := range items {
		e := &items[i]
		if !isEphemeral(e) || !e.DeletionTimestamp.IsZero() || e.Status.Phase == doblurav1alpha1.EnvExpired {
			continue
		}
		if !match(e) {
			continue
		}
		out = append(out, e.Namespace+"/"+e.Name)
	}
	sort.Strings(out)
	return out
}

// describe renders the offending environments into the refusal, because the first
// thing anybody asks on being refused is "which ones?" and the answer should not
// require a second command.
func describe(names []string) string {
	const most = 5
	if len(names) > most {
		return strings.Join(names[:most], ", ") + fmt.Sprintf(" and %d more", len(names)-most)
	}
	return strings.Join(names, ", ")
}

// isEphemeral reads the lifecycle, treating the empty value as Ephemeral.
//
// The CRD defaults the field, so anything from the API server has it set. The
// empty case is a struct built in Go — a test, or a client on an older type — and
// answering "Ephemeral" there matches what the API server would have done rather
// than quietly exempting the object from the quota.
func isEphemeral(env *doblurav1alpha1.OdooEnvironment) bool {
	t := env.Spec.Lifecycle.Type
	return t == "" || t == doblurav1alpha1.LifecycleEphemeral
}
