// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Antoni Romera

// Package webhook holds Doblura's admission webhooks and the certificate
// plumbing they need.
package webhook

import (
	"context"
	"fmt"
	"k8s.io/apimachinery/pkg/api/equality"
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
	if m.Client == nil {
		return nil, ""
	}

	// An environment that names no customer takes the default one, if there is a
	// default one. It exists for the company that runs its own Odoo rather than
	// forty of somebody else's: without it, that company gets none of the
	// platform — no image catalogue, no generated address, no certificate issuer,
	// no defaults — unless it writes forTenant on every environment for ever.
	//
	// The patch is written to the SPEC, so what the object says is what happened.
	// Resolving it invisibly at reconcile time would leave an environment whose
	// yaml names no customer and whose behaviour comes from one.
	var ops []jsonpatch.Operation
	name := env.Spec.ForTenant
	if name == "" {
		def, err := m.defaultTenant(ctx, env.Namespace)
		switch {
		case err != nil:
			return nil, err.Error()
		case def == "":
			// No default and none named: an internal environment, as before. The
			// handover guardrail deliberately does not apply to it.
			return nil, ""
		}
		name = def
		ops = append(ops, jsonpatch.NewOperation("add", "/spec/forTenant", name))
	}

	var tenant doblurav1alpha1.OdooTenant
	if err := m.Client.Get(ctx, client.ObjectKey{
		Namespace: env.Namespace, Name: name,
	}, &tenant); err != nil {
		// Deliberately not an error. A missing customer record is caught by the
		// validating half with a message about the tenant; failing here would
		// report it as a defaulting failure, which names the wrong thing.
		return nil, ""
	}

	// An imageRef that does not resolve is REFUSED, never quietly replaced by the
	// customer's default. Falling back was the first implementation and it was
	// wrong in the worst available way: a typo in a catalogue name produced a
	// working environment running a different version of the product than the
	// person asked for, with nothing anywhere saying so.
	// Choosing a version must not lose everything else the customer record says.
	// This block used to RETURN as soon as imageRef resolved, so naming a
	// catalogue entry skipped the database, the storage and the size — and the
	// environment then failed schema validation on spec.database, pointing at a
	// field the person had never had to fill in before.
	var chosen *doblurav1alpha1.ImageCatalogueEntry
	switch {
	case env.Spec.ImageRef != "":
		chosen = tenant.Spec.ImageByName(env.Spec.ImageRef)
		if chosen == nil {
			return nil, fmt.Sprintf(
				"%q is not in %s's image catalogue. Available: %s",
				env.Spec.ImageRef, tenant.Name, catalogueNames(&tenant))
		}
	case env.Spec.Image == "":
		// The catalogue's default wins over environmentDefaults.image: the
		// catalogue is what somebody edits when they change versions, and the
		// other is a leftover from before there was one.
		chosen = tenant.Spec.DefaultImage()
	}

	// The purpose expands FIRST, so the customer's own defaults can still
	// override it: a customer whose staging must be on a ReadWriteMany claim says
	// so once, and the purpose does not undo it.
	ops = append(ops, purposeOps(env)...)

	// What the customer's data is, and the handful of things that follow from it.
	//
	// Refused here rather than described in a policy document, which is the whole
	// difference between a control and a paragraph. See DataHeld for why these
	// specific ones and not a general compliance mode.
	if refusal := dataRules(env, &tenant); refusal != "" {
		return nil, refusal
	}
	if kinds := tenant.Spec.Holds.Kinds(); len(kinds) > 0 {
		// Recorded on the environment so evidence does not have to be inferred
		// later from a customer record that may have changed since. A LABEL and
		// not an annotation, because the question it answers is "show me
		// everything holding personal data", and only labels can be selected on.
		//
		// RFC 6902 refuses to add a member to an object that does not exist, so
		// an environment with no labels at all needs the whole map created in one
		// operation — the same trap the creator annotation already fell into.
		value := strings.Join(kinds, "_")
		if env.Labels == nil {
			ops = append(ops, jsonpatch.NewOperation("add", "/metadata/labels",
				map[string]string{dataLabel: value}))
		} else {
			ops = append(ops, jsonpatch.NewOperation("add",
				"/metadata/labels/"+escapeJSONPointer(dataLabel), value))
		}
	}

	// The address, generated once and written into the spec.
	//
	// Into the SPEC and not resolved at reconcile time, because a hostname
	// recomputed on every reconcile is a hostname that moves under a running
	// certificate and under whatever DNS points at it. Written here it is the
	// same address for the life of the environment, and it is visible in the
	// object somebody reads.
	if hostOp, refusal := hostFor(env, &tenant); refusal != "" {
		return nil, refusal
	} else if hostOp != nil {
		ops = append(ops, *hostOp)
	}

	d := tenant.Spec.EnvironmentDefaults

	switch {
	case chosen != nil:
		ops = append(ops, jsonpatch.NewOperation("add", "/spec/image", chosen.Image))
		if chosen.Flavor != "" {
			// The flavour follows the image it belongs to. Written even when it
			// equals the schema default, so the object records what was decided
			// rather than what happened to be left unset.
			ops = append(ops, jsonpatch.NewOperation("add", "/spec/imageFlavor", string(chosen.Flavor)))
		}
		// And the addons directories the image turned out to have, from the study
		// rather than from a convention.
		//
		// This is what closes the loop an OdooBuild opens. A built image carries
		// its modules in a directory the flavour list knows nothing about, so
		// without this somebody has to copy that path into spec.addons.baked by
		// hand — and a value that has to be transcribed is a value that will be
		// transcribed wrong, on the field whose failure mode is Odoo starting
		// happily and then not having the module.
		//
		// From the STUDY, which observed the directory by running the image, and
		// never from a guess: an addons_path entry that does not exist is exactly
		// the failure envpod.go warns about.
		if extra := studiedAddons(&tenant, chosen, env); len(extra) > 0 {
			// RFC 6902 refuses to add a member to an object that does not exist,
			// and `spec.addons` is absent on an environment that declared none —
			// schema defaults do not conjure the parent. So the whole object goes
			// in when there is nothing there, and only the member when there is.
			// The same trap the creator annotation and the data label both fell
			// into; this is the third.
			if equality.Semantic.DeepEqual(env.Spec.Addons, doblurav1alpha1.AddonsSpec{}) {
				ops = append(ops, jsonpatch.NewOperation("add", "/spec/addons",
					map[string]any{"baked": extra}))
			} else {
				ops = append(ops, jsonpatch.NewOperation("add", "/spec/addons/baked", extra))
			}
		}
	case env.Spec.Image == "" && d != nil && d.Image != "":
		ops = append(ops, jsonpatch.NewOperation("add", "/spec/image", d.Image))
	}

	if d == nil {
		return ops, ""
	}
	if env.Spec.Database.Host == "" && d.Database != nil {
		ops = append(ops, jsonpatch.NewOperation("add", "/spec/database", d.Database))
	}
	if (env.Spec.Storage == nil || env.Spec.Storage.Filestore == nil) && d.Storage != nil {
		ops = append(ops, jsonpatch.NewOperation("add", "/spec/storage", d.Storage))
	}
	// Whole-object, never merged. A customer declaring six repositories and an
	// environment declaring one means the environment wants ONE — merging would
	// give it seven, including five it did not ask for, and there would be no way
	// to express "just this repository" at all.
	if len(env.Spec.Addons.Repos) == 0 && env.Spec.Addons.Volume == nil &&
		len(env.Spec.Addons.Baked) == 0 && d.Addons != nil {
		ops = append(ops, jsonpatch.NewOperation("add", "/spec/addons", d.Addons))
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

// purposeOps expands spec.purpose into the fields it implies.
//
// Only where they are empty, and never over something the person wrote. The
// purpose is the answer to "what is this for", and the fields are how that is
// achieved; somebody who knows they want a Persistent review environment is not
// wrong, they are being specific.
//
// This runs before the customer's environmentDefaults are applied, so a customer
// with an unusual requirement — staging on a ReadWriteMany claim, say — declares
// it once on their record and the purpose does not undo it.
func purposeOps(env *doblurav1alpha1.OdooEnvironment) []jsonpatch.Operation {
	want, ok := doblurav1alpha1.DefaultsFor(env.Spec.Purpose)
	if !ok {
		return nil
	}

	var ops []jsonpatch.Operation

	// The lifecycle object may be absent entirely, in which case it has to be
	// added whole: RFC 6902 refuses to add a member to an object that is not
	// there, which is the same trap the creator annotation hit.
	if env.Spec.Lifecycle.Type == "" {
		life := map[string]any{"type": string(want.Lifecycle)}
		if want.TTL != "" {
			life["ttl"] = want.TTL
		}
		ops = append(ops, jsonpatch.NewOperation("replace", "/spec/lifecycle", life))
	}

	if env.Spec.Data.Type == "" {
		ops = append(ops, jsonpatch.NewOperation("replace", "/spec/data",
			map[string]any{"type": string(want.Data)}))
	}

	if env.Spec.Storage == nil || env.Spec.Storage.Filestore == nil {
		fs := map[string]any{"mode": string(want.Filestore)}
		// A PersistentVolumeClaim filestore needs a size or a claim name, and
		// the API refuses one with neither. Production is the only purpose that
		// asks for a volume, and giving it a size it can grow past is better
		// than giving it a rule it trips over on the first apply.
		if want.Filestore == doblurav1alpha1.FilestorePVC {
			fs["size"] = "20Gi"
		}
		ops = append(ops, jsonpatch.NewOperation("replace", "/spec/storage",
			map[string]any{"filestore": fs}))
	}

	if env.Spec.Size == "" && want.Size != "" {
		ops = append(ops, jsonpatch.NewOperation("add", "/spec/size", string(want.Size)))
	}
	return ops
}

// hostFor decides where an environment answers.
//
// Three cases, and the third is the one that matters:
//
//   - A host was given: it stands. Somebody naming an address means it.
//   - No host, and the customer has a domain: generate one under it, with a
//     random tail. See OdooTenantSpec.Domain for why random.
//   - Production with no host: REFUSED. Production is the customer's real
//     address; guessing it is worse than asking, and an environment answering on
//     a name nobody chose is not a production environment anybody should trust.
func hostFor(
	env *doblurav1alpha1.OdooEnvironment,
	tenant *doblurav1alpha1.OdooTenant,
) (*jsonpatch.Operation, string) {
	if env.Spec.Exposure.Host != "" {
		return nil, ""
	}

	// Production is never generated, on any path. It is the customer's real
	// address; guessing it is worse than asking, and an environment answering on
	// a name nobody chose is not a production environment anybody should trust.
	//
	// The refusal only fires when it is public, because a production environment
	// reached some other way needs no host at all. The CEL rule on exposure
	// already refuses public-with-no-host; this says the extra thing that rule
	// cannot, which is that doblura declined to make one up.
	if env.Spec.Purpose == doblurav1alpha1.PurposeProduction {
		if env.Spec.Exposure.Public != nil && *env.Spec.Exposure.Public {
			return nil, fmt.Sprintf(
				"a Production environment needs its own exposure.host: it is %s's "+
					"real address, and doblura will not invent one. Every other "+
					"purpose gets a generated address under the customer's domain",
				tenant.Name)
		}
		return nil, ""
	}

	if tenant.Spec.Domain == "" {
		// Nothing to build from. Left empty rather than invented; the CEL rule
		// refuses a public environment with no host, which is the right place for
		// that refusal and says so in those words.
		return nil, ""
	}

	host, err := doblurav1alpha1.GeneratedHost(env.Name, tenant.Spec.Domain)
	if err != nil {
		// Only a failure of crypto/rand reaches here. Refused rather than falling
		// back to something predictable: a name nobody can type is the point.
		return nil, "could not generate an address for this environment: " + err.Error()
	}
	if host == "" {
		return nil, ""
	}

	// Add the whole exposure object when the manifest had none.
	//
	// A JSON Patch "add" of /spec/exposure/host fails outright if /spec/exposure
	// is absent, and it is absent whenever the person did not write it — schema
	// defaults for the fields INSIDE an object are only applied when the object
	// itself exists. NoIndex is the tell: it defaults to true, so a nil there
	// means the API server never saw an exposure to default.
	if env.Spec.Exposure.NoIndex == nil {
		op := jsonpatch.NewOperation("add", "/spec/exposure",
			map[string]any{"host": host})
		return &op, ""
	}
	op := jsonpatch.NewOperation("add", "/spec/exposure/host", host)
	return &op, ""
}

// defaultTenant is the customer record marked as the default in this namespace.
//
// At most one. Two would make which defaults apply depend on iteration order,
// which is the kind of thing that is right for months and then is not — so this
// refuses rather than picking, and the message names both so somebody can fix it
// without going looking.
func (m *EnvironmentCreator) defaultTenant(ctx context.Context, ns string) (string, error) {
	var list doblurav1alpha1.OdooTenantList
	if err := m.Client.List(ctx, &list, client.InNamespace(ns)); err != nil {
		// Not fatal: an environment that names its own customer does not reach
		// here, and one that does not simply stays without one, as before.
		return "", nil //nolint:nilerr // absence of a default is not an error
	}
	found := ""
	for i := range list.Items {
		t := &list.Items[i]
		if !t.Spec.IsDefault() {
			continue
		}
		if found != "" {
			return "", fmt.Errorf(
				"%s and %s are both marked as the default customer in %s, so which "+
					"one an environment inherits would depend on the order they came "+
					"back in. Mark one", found, t.Name, ns)
		}
		found = t.Name
	}
	return found, nil
}

// dataRules refuses the configurations that are wrong for what the data is.
//
// Three of them, and the shortness is the point: doblura enforces what it can be
// certain of whatever version of whichever standard applies, and says nothing
// about the rest. A "compliance mode" that switched on twenty settings would be
// claiming knowledge of an audit doblura has not read.
// dataLabel records what kinds of data an environment holds, for selecting on.
const dataLabel = "doblura.dev/data"

// studiedAddons is the addons directories this image was OBSERVED to have, minus
// the ones the flavour already puts on the path and the ones already declared.
//
// Empty when there is nothing to add, so an environment that needs no patch does
// not get one — a no-op patch still rewrites the field and shows up as a change
// in every audit of the object.
func studiedAddons(
	tenant *doblurav1alpha1.OdooTenant,
	chosen *doblurav1alpha1.ImageCatalogueEntry,
	env *doblurav1alpha1.OdooEnvironment,
) []string {
	var study *doblurav1alpha1.ImageStudy
	for i := range tenant.Status.ImageStudies {
		if tenant.Status.ImageStudies[i].Name == chosen.Name &&
			tenant.Status.ImageStudies[i].Image == chosen.Image {
			study = &tenant.Status.ImageStudies[i]
			break
		}
	}
	if study == nil {
		return nil
	}

	have := map[string]bool{}
	for _, p := range doblurav1alpha1.FlavorBakedPaths(chosen.Flavor) {
		have[p] = true
	}
	for _, p := range env.Spec.Addons.Baked {
		have[p] = true
	}

	// Odoo's own package path is left out, and that needed measuring rather than
	// assuming — flavor_test.go says an addons_path "REPLACES the default rather
	// than adding to it", which is true of the IMAGE's declared addons_path and
	// not of Odoo's core:
	//
	//	odoo -c <conf with addons_path = /opt/doblura/addons>
	//	  → addons paths: ['/usr/lib/python3/dist-packages/odoo/addons',
	//	                   '/var/lib/odoo/addons/18.0', '/opt/doblura/addons']
	//
	// Odoo adds its package and data_dir/addons/<series> whatever the config
	// says. What IS lost is the image's own value — an empty /mnt/extra-addons in
	// the official image, and /opt/odoo/auto/addons in Doodba, which is precisely
	// why FlavorBakedPaths exists. So the studied directories are carried over and
	// the package path is not: putting it in the field would say doblura decided
	// something it did not.
	out := append([]string{}, env.Spec.Addons.Baked...)
	added := false
	for _, p := range study.AddonsPaths {
		if have[p] || strings.Contains(p, "/dist-packages/") {
			continue
		}
		have[p] = true
		out = append(out, p)
		added = true
	}
	if !added {
		return nil
	}
	return out
}

func dataRules(
	env *doblurav1alpha1.OdooEnvironment,
	tenant *doblurav1alpha1.OdooTenant,
) string {
	held := tenant.Spec.Holds
	production := env.Spec.Purpose == doblurav1alpha1.PurposeProduction

	if held.HasCardholderData() && !production {
		// Live data outside production, refused with no way past it.
		//
		// A scope argument rather than a safety one, and that is what to say to
		// whoever wants the exception: the moment cardholder data reaches a
		// staging environment, that environment is in the audit's scope — and so
		// is its cluster, its backups, and everybody who can read them. Refusing
		// the copy is cheaper than auditing the copy.
		if env.Spec.Data.Type == doblurav1alpha1.DataLive {
			return fmt.Sprintf(
				"%s holds cardholder data, so data.type Live is only allowed in a "+
					"Production environment. This one is %s. There is deliberately "+
					"no acknowledgement for this: a copy of cardholder data puts the "+
					"environment holding it, its cluster and its backups inside the "+
					"audit's scope, and refusing the copy costs less than auditing "+
					"it. Use Snapshot, which is anonymised",
				tenant.Name, purposeOrUnset(env))
		}
		// And mail, because a non-production environment that can send is one
		// that can send to real cardholders from a machine nobody is watching.
		if env.Spec.Mail != nil {
			return fmt.Sprintf(
				"%s holds cardholder data, so outgoing mail is only configured on "+
					"its Production environment. This one is %s",
				tenant.Name, purposeOrUnset(env))
		}
	}

	if held.HasPersonalData() {
		// noIndex cannot be turned off. An environment holding data about
		// identifiable people, indexed by a search engine, is a disclosure that
		// required nobody to attack anything — and one that is reportable within
		// 72 hours in most of Europe.
		if env.Spec.Exposure.NoIndex != nil && !*env.Spec.Exposure.NoIndex {
			return fmt.Sprintf(
				"%s holds personal data, so exposure.noIndex cannot be turned off: "+
					"an environment with real people's details indexed by a search "+
					"engine is a disclosure nobody had to attack anything to get, "+
					"and it is reportable", tenant.Name)
		}
	}

	return ""
}

func purposeOrUnset(env *doblurav1alpha1.OdooEnvironment) string {
	if env.Spec.Purpose == "" {
		return "not marked with a purpose at all"
	}
	return string(env.Spec.Purpose)
}
