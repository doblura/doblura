// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Antoni Romera

package controller

import (
	"context"
	"fmt"
	"sort"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	doblurav1alpha1 "github.com/doblura/doblura/api/v1alpha1"
)

// RunboatLinkReconciler mirrors a Runboat's builds and proxies allowed actions.
//
// It is the only reconciler here that talks to something outside the cluster on
// a timer rather than creating Kubernetes objects, which makes it the only one
// whose correctness depends on staleness. So the two things it is careful about
// are: never dropping the mirror because one poll failed, and never executing an
// action twice.
type RunboatLinkReconciler struct {
	client.Client
	Scheme *runtime.Scheme

	// API is the Runboat client. Left nil it builds the real one; the tests
	// substitute a fake.
	API runboatAPI
}

// Note the absence of a NetworkPolicy here, and of anything creating workloads.
// This controller makes outbound HTTP calls from the manager pod itself — the
// only place in Doblura that does. Worth knowing when you write egress rules for
// the manager's namespace: block it and the mirror simply never fills, with
// "cannot reach runboat" in the status and no other symptom.

// +kubebuilder:rbac:groups=doblura.dev,resources=runboatlinks,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=doblura.dev,resources=runboatlinks/status,verbs=get;update;patch
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch

func (r *RunboatLinkReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	lg := log.FromContext(ctx)

	var link doblurav1alpha1.RunboatLink
	if err := r.Get(ctx, req.NamespacedName, &link); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	api := r.API
	if api == nil {
		api = newHTTPRunboat()
	}

	st := link.Status.DeepCopy()
	st.ObservedGeneration = link.Generation
	every := link.Spec.PollEvery().Duration

	creds, err := r.credentials(ctx, &link)
	if err != nil {
		// A missing Secret is a configuration error, not a transient one, so it is
		// reported and retried on the poll interval rather than hammered with
		// exponential backoff on an error that will not fix itself.
		meta.SetStatusCondition(&st.Conditions, metav1.Condition{
			Type: doblurav1alpha1.ConditionRunboatReachable, Status: metav1.ConditionFalse,
			Reason: "CredentialsUnavailable", Message: err.Error(),
		})
		st.Message = err.Error()
		return r.finish(ctx, &link, st, every)
	}

	// ── Poll ──
	remote, pollErr := api.Builds(ctx, &link.Spec, creds)
	if pollErr != nil {
		// The mirror is deliberately NOT cleared. Blanking it would say every build
		// disappeared, when what happened is that Doblura stopped being able to see
		// them — two very different facts, and the interface shows the second one
		// as a staleness warning over the last known list.
		meta.SetStatusCondition(&st.Conditions, metav1.Condition{
			Type: doblurav1alpha1.ConditionRunboatReachable, Status: metav1.ConditionFalse,
			Reason: "PollFailed", Message: pollErr.Error(),
		})
		r.setFreshness(st, every)
		st.Message = pollErr.Error()
		lg.Info("runboat poll failed, keeping the previous mirror",
			"builds", len(st.Builds), "error", pollErr.Error())
		return r.finish(ctx, &link, st, every)
	}

	kept, total := mirrorBuilds(&link.Spec, remote)
	st.Builds = kept
	st.Total = total
	st.Truncated = int(total) > len(kept)
	now := metav1.Now()
	st.LastPoll = &now
	meta.SetStatusCondition(&st.Conditions, metav1.Condition{
		Type: doblurav1alpha1.ConditionRunboatReachable, Status: metav1.ConditionTrue,
		Reason: "Polled", Message: fmt.Sprintf("%d builds", total),
	})
	r.setFreshness(st, every)

	st.Message = fmt.Sprintf("mirroring %d of %d builds", len(kept), total)
	if st.Truncated {
		// Said out loud, in the message and in a printcolumn. A capped list reads
		// exactly like a complete one, and that is the whole danger.
		st.Message += fmt.Sprintf(" (capped at spec.maxBuilds=%d)", link.Spec.MaxBuilds)
	}

	// ── Execute new action requests ──
	//
	// After the poll, so a request naming a build that has just vanished fails
	// against a fresh mirror instead of an old one.
	for _, want := range link.Spec.ActionRequests {
		if runboatAlreadyDone(st.ExecutedActions, want.ID) {
			continue
		}
		res := doblurav1alpha1.RunboatActionResult{
			ID: want.ID, Build: want.Build, Action: want.Action,
			RequestedBy: want.RequestedBy,
		}
		at := metav1.Now()
		res.ExecutedAt = &at

		switch {
		case !link.Spec.Allows(want.Action):
			// Also enforced by a CEL rule on apply. Kept here because the rule can
			// only see one object: allowedActions could be narrowed after a request
			// was already accepted, and the narrowing has to win.
			res.Succeeded = false
			res.Message = fmt.Sprintf("action %s is not in spec.allowedActions", want.Action)
		case !mirrorHasBuild(st.Builds, want.Build):
			res.Succeeded = false
			res.Message = fmt.Sprintf("no build named %q in the mirror; it may have been undeployed", want.Build)
		default:
			if err := api.Act(ctx, &link.Spec, creds, want.Build, want.Action); err != nil {
				res.Succeeded = false
				res.Message = err.Error()
			} else {
				res.Succeeded = true
			}
		}

		lg.Info("runboat action", "id", want.ID, "build", want.Build,
			"action", want.Action, "by", want.RequestedBy, "ok", res.Succeeded)

		// Recorded whether it worked or not. A failed attempt must still be
		// remembered: retrying a Reset on its own, without somebody asking again,
		// would be the controller deciding to wipe a database twice.
		st.ExecutedActions = appendExecuted(st.ExecutedActions, res)
	}

	return r.finish(ctx, &link, st, every)
}

// setFreshness records whether the mirror can be trusted as current.
//
// Two poll intervals of slack, so one missed tick is not an alarm. The condition
// exists because everything else in this status is a copy, and a copy is worth
// exactly what its age says it is.
func (r *RunboatLinkReconciler) setFreshness(st *doblurav1alpha1.RunboatLinkStatus, every time.Duration) {
	fresh := st.LastPoll != nil && time.Since(st.LastPoll.Time) < 2*every
	cond := metav1.Condition{
		Type: doblurav1alpha1.ConditionMirrorFresh, Status: metav1.ConditionTrue,
		Reason: "Current", Message: "refreshed within two poll intervals",
	}
	if !fresh {
		cond.Status = metav1.ConditionFalse
		cond.Reason = "Stale"
		cond.Message = "the builds shown may no longer exist"
		if st.LastPoll == nil {
			cond.Message = "never polled successfully"
		}
	}
	meta.SetStatusCondition(&st.Conditions, cond)
}

// finish writes the status only when it changed, and requeues for the next poll.
//
// The comparison is what keeps this controller from being a write loop: it runs
// on a timer, so without it every tick would be an etcd write and every write
// would wake the watch.
func (r *RunboatLinkReconciler) finish(
	ctx context.Context,
	link *doblurav1alpha1.RunboatLink,
	st *doblurav1alpha1.RunboatLinkStatus,
	every time.Duration,
) (ctrl.Result, error) {
	if !equality.Semantic.DeepEqual(&link.Status, st) {
		link.Status = *st
		if err := r.Status().Update(ctx, link); err != nil {
			return ctrl.Result{}, err
		}
	}
	return ctrl.Result{RequeueAfter: every}, nil
}

// credentials resolves spec.auth into a usable pair.
//
// Returns nil, nil when no auth is configured: listing builds does not need it,
// and demanding a credential to look would make the read-only case harder than
// it is.
func (r *RunboatLinkReconciler) credentials(ctx context.Context, link *doblurav1alpha1.RunboatLink) (*RunboatCredentials, error) {
	if link.Spec.Auth == nil {
		return nil, nil
	}
	var sec corev1.Secret
	key := types.NamespacedName{Namespace: link.Namespace, Name: link.Spec.Auth.BasicAuthSecret}
	if err := r.Get(ctx, key, &sec); err != nil {
		return nil, fmt.Errorf("cannot read secret %s: %w", link.Spec.Auth.BasicAuthSecret, err)
	}
	u, okU := sec.Data["username"]
	p, okP := sec.Data["password"]
	if !okU || !okP {
		// Named explicitly. "authentication failed" against a Secret that is simply
		// missing a key is an afternoon of looking in the wrong place.
		return nil, fmt.Errorf("secret %s must hold keys \"username\" and \"password\"",
			link.Spec.Auth.BasicAuthSecret)
	}
	return &RunboatCredentials{Username: string(u), Password: string(p)}, nil
}

// mirrorBuilds filters, sorts and caps what goes into status.
//
// Returns the kept builds and the total that matched, so the caller can tell
// whether the cap was hit. Newest first, because the interesting build is
// essentially always the most recent one.
func mirrorBuilds(spec *doblurav1alpha1.RunboatLinkSpec, remote []RunboatRemoteBuild) ([]doblurav1alpha1.RunboatBuild, int32) {
	out := make([]doblurav1alpha1.RunboatBuild, 0, len(remote))
	for _, b := range remote {
		// Filtered here even when the query already narrowed it server-side: the
		// server-side form only covers a single repo, so this is the general case
		// and the query is the optimisation.
		if !spec.MatchesFilter(b.CommitInfo.Repo, b.CommitInfo.TargetBranch) {
			continue
		}
		out = append(out, toMirror(b))
	}
	total := int32(len(out))

	sort.SliceStable(out, func(i, j int) bool {
		ti, tj := out[i].Created, out[j].Created
		switch {
		case ti == nil && tj == nil:
			// Runboat sent no usable timestamp for either. Falling back to the name
			// keeps the order stable across polls, which matters more than it looks:
			// an unstable order means a status that differs every tick, so it is
			// written every tick, and the write-loop guard stops working.
			return out[i].Name < out[j].Name
		case ti == nil:
			return false
		case tj == nil:
			return true
		}
		return ti.Time.After(tj.Time)
	})

	cap32 := spec.MaxBuilds
	if cap32 <= 0 {
		cap32 = 200
	}
	if total > cap32 {
		out = out[:cap32]
	}
	return out, total
}

func mirrorHasBuild(builds []doblurav1alpha1.RunboatBuild, name string) bool {
	for _, b := range builds {
		if b.Name == name {
			return true
		}
	}
	return false
}

func runboatAlreadyDone(done []doblurav1alpha1.RunboatActionResult, id string) bool {
	for _, d := range done {
		if d.ID == id {
			return true
		}
	}
	return false
}

// appendExecuted adds a result, keeping the list bounded.
//
// The bound is in the CRD as MaxItems=64, and a status that exceeds its own
// schema is rejected on write — which would fail the whole reconcile over a log
// entry. So the oldest entries are dropped here.
//
// Dropping the oldest also expires the idempotency memory: an id evicted from
// this list could be executed again if it were still in spec.actionRequests. It
// takes 64 further requests to get there, and the alternative — an unbounded
// list — eventually makes the object unwritable. Documented rather than hidden.
func appendExecuted(done []doblurav1alpha1.RunboatActionResult, res doblurav1alpha1.RunboatActionResult) []doblurav1alpha1.RunboatActionResult {
	const maxExecuted = 64
	done = append(done, res)
	if len(done) > maxExecuted {
		done = done[len(done)-maxExecuted:]
	}
	return done
}

func (r *RunboatLinkReconciler) SetupWithManager(mgr ctrl.Manager) error {
	// No GenerationChangedPredicate here, unlike every other reconciler in this
	// package. This one drives itself with RequeueAfter, and it writes status on
	// most ticks; the write-loop defence is the DeepEqual in finish(). Adding the
	// predicate as well would be harmless but misleading — it would suggest the
	// polling comes from watch events.
	return ctrl.NewControllerManagedBy(mgr).
		For(&doblurav1alpha1.RunboatLink{}).
		Named("runboatlink").
		Complete(r)
}
