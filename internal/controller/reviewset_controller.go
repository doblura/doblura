// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Antoni Romera

package controller

import (
	"context"
	"crypto/sha1" //nolint:gosec // a short stable suffix, not a security boundary
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	doblurav1alpha1 "github.com/doblura/doblura/api/v1alpha1"
)

// OdooReviewSetReconciler keeps environments in step with a repository.
//
// The loop is: ask the forge what is open, work out what should exist, create
// what is missing, delete what is no longer wanted. Everything about HOW an
// environment is built comes from the customer's record, exactly as it does when
// somebody creates one by hand — this decides only which ones exist.
//
// Deleting is the half that needs care. An environment created for a pull request
// that has since closed should go; an environment somebody created by hand, with
// a name that happens to look similar, must not. So this deletes only what it
// owns, established by an owner reference and a label it set itself, and never by
// matching a name.
type OdooReviewSetReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// The label that says an environment belongs to a set.
const (
	reviewSetLabel = "doblura.dev/review-set"
	reviewRefLabel = "doblura.dev/review-ref"
)

// +kubebuilder:rbac:groups=doblura.dev,resources=reviewsets,verbs=get;list;watch
// +kubebuilder:rbac:groups=doblura.dev,resources=reviewsets/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=doblura.dev,resources=odooenvironments,verbs=get;list;watch;create;delete

func (r *OdooReviewSetReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	l := log.FromContext(ctx)

	var set doblurav1alpha1.ReviewSet
	if err := r.Get(ctx, req.NamespacedName, &set); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	st := set.Status.DeepCopy()
	st.ObservedGeneration = set.Generation

	every := time.Minute
	if p := set.Spec.PollInterval; p != nil && p.Duration > 0 {
		every = p.Duration
	}

	if set.Spec.Paused {
		// Paused stops the loop and says so, rather than looking like a set that
		// has stopped working. Nothing is deleted: pausing is what somebody does
		// when they want the noise to stop, not when they want the environments
		// gone.
		meta.SetStatusCondition(&st.Conditions, metav1.Condition{
			Type: "Watching", Status: metav1.ConditionFalse, Reason: "Paused",
			Message: "paused; no environment is being created or removed",
			ObservedGeneration: set.Generation,
		})
		return r.commitSet(ctx, &set, st, ctrl.Result{RequeueAfter: every})
	}

	token, err := r.forgeToken(ctx, &set)
	if err != nil {
		return r.failSet(ctx, &set, st, "CredentialUnreadable", err, every)
	}

	refs, err := listRefs(ctx, set.Spec.Repository, set.Spec.Watch, token)
	if err != nil {
		return r.failSet(ctx, &set, st, "ForgeUnreachable", err, every)
	}
	now := metav1.Now()
	st.LastPolled = &now

	// Labels narrow pull requests to the ones somebody marked. Applied here and
	// not in the query, because GitHub and GitLab spell label filters
	// differently and a wrong filter returns an empty list that looks exactly
	// like "nothing is open".
	if len(set.Spec.Watch.Labels) > 0 {
		kept := refs[:0]
		for _, ref := range refs {
			if ref.Kind == "Branch" || hasAnyLabel(ref.Labels, set.Spec.Watch.Labels) {
				kept = append(kept, ref)
			}
		}
		refs = kept
	}

	// A stable order, so the cap always drops the same ones and a set that is
	// over its limit does not churn: creating and deleting the same environments
	// on every pass would be worse than skipping them.
	sort.Slice(refs, func(i, j int) bool {
		if refs[i].Kind != refs[j].Kind {
			return refs[i].Kind < refs[j].Kind
		}
		if refs[i].Number != refs[j].Number {
			return refs[i].Number < refs[j].Number
		}
		return refs[i].Ref < refs[j].Ref
	})

	var mine doblurav1alpha1.OdooEnvironmentList
	if err := r.List(ctx, &mine, client.InNamespace(set.Namespace),
		client.MatchingLabels{reviewSetLabel: set.Name}); err != nil {
		return ctrl.Result{}, err
	}

	wanted := make(map[string]forgeRef, len(refs))
	var tracked []doblurav1alpha1.TrackedRef
	var skipped int32
	for _, ref := range refs {
		if int32(len(wanted)) >= set.Spec.MaxEnvironments { //nolint:gosec // bounded by the schema
			skipped++
			continue
		}
		name := envNameFor(&set, ref)
		wanted[name] = ref
		tracked = append(tracked, doblurav1alpha1.TrackedRef{
			Name: name, Kind: ref.Kind, Ref: ref.Ref,
			Number: ref.Number, Title: ref.Title, URL: ref.URL,
		})
	}

	// Remove first, so a set at its cap can replace a closed pull request with an
	// open one in the same pass rather than waiting a poll interval.
	for i := range mine.Items {
		e := &mine.Items[i]
		if _, keep := wanted[e.Name]; keep {
			continue
		}
		if err := r.Delete(ctx, e); err != nil && !errors.IsNotFound(err) {
			return ctrl.Result{}, err
		}
		l.Info("removed a review environment whose ref is gone",
			"environment", e.Name, "set", set.Name)
	}

	for name, ref := range wanted {
		if err := r.ensureReviewEnvironment(ctx, &set, name, ref); err != nil {
			return r.failSet(ctx, &set, st, "CannotCreate", err, every)
		}
	}

	st.Tracked = tracked
	st.Open = int32(len(wanted)) //nolint:gosec // bounded by MaxEnvironments
	st.Skipped = skipped
	st.Message = ""

	msg := fmt.Sprintf("%d open", len(wanted))
	if skipped > 0 {
		// Said out loud. A set quietly ignoring half its pull requests looks
		// exactly like one that is working.
		msg = fmt.Sprintf("%s; %d skipped, at the cap of %d",
			msg, skipped, set.Spec.MaxEnvironments)
	}
	meta.SetStatusCondition(&st.Conditions, metav1.Condition{
		Type: "Watching", Status: metav1.ConditionTrue, Reason: "Polled",
		Message: msg, ObservedGeneration: set.Generation,
	})

	return r.commitSet(ctx, &set, st, ctrl.Result{RequeueAfter: every})
}

// ensureReviewEnvironment creates one, and never updates it.
//
// An environment that already exists is left alone: repointing a running review
// environment at a force-pushed branch would restore over a database somebody may
// be looking at, and the operator has no way to know they are not. A new commit
// on the same branch is picked up when the pod restarts, which is the behaviour
// people expect from a review environment.
func (r *OdooReviewSetReconciler) ensureReviewEnvironment(
	ctx context.Context,
	set *doblurav1alpha1.ReviewSet,
	name string,
	ref forgeRef,
) error {
	var existing doblurav1alpha1.OdooEnvironment
	err := r.Get(ctx, client.ObjectKey{Namespace: set.Namespace, Name: name}, &existing)
	if err == nil {
		return nil
	}
	if !errors.IsNotFound(err) {
		return err
	}

	t := set.Spec.Template
	repoName := t.RepoName
	if repoName == "" {
		repoName = set.Name
	}

	env := &doblurav1alpha1.OdooEnvironment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: set.Namespace,
			Labels: map[string]string{
				reviewSetLabel: set.Name,
				reviewRefLabel: shortHash(ref.Kind + "/" + ref.Ref),
			},
			Annotations: map[string]string{
				"doblura.dev/review-title": ref.Title,
				"doblura.dev/review-url":   ref.URL,
			},
		},
		Spec: doblurav1alpha1.OdooEnvironmentSpec{
			ForTenant: set.Spec.ForTenant,
			Purpose:   t.Purpose,
			ImageRef:  t.ImageRef,
			Addons: doblurav1alpha1.AddonsSpec{
				Repos: []doblurav1alpha1.AddonRepo{{
					Name:  repoName,
					URL:   set.Spec.Repository.URL,
					Ref:   ref.Ref,
					Paths: t.Paths,
					Auth:  set.Spec.Repository.Auth,
					Depth: 1,
				}},
			},
		},
	}
	if t.TTL != nil {
		env.Spec.Lifecycle = doblurav1alpha1.EnvLifecycle{
			Type: doblurav1alpha1.LifecycleEphemeral, TTL: t.TTL,
		}
	}
	if err := ctrl.SetControllerReference(set, env, r.Scheme); err != nil {
		return err
	}
	// Created plainly, so the quota webhook sees it as a create and the
	// admission rules apply exactly as they do to a person's.
	if err := r.Create(ctx, env); err != nil && !errors.IsAlreadyExists(err) {
		return err
	}
	return nil
}

// envNameFor is the environment's name, and it has to be stable.
//
// A pull request gives a number, which is short and unique and survives a
// retitle. A branch gives a name that may be long, may contain slashes, and may
// not be a DNS label at all — so it is sanitised and given a short hash of the
// original, because `feature/ACME-1` and `feature-acme-1` must not collide.
func envNameFor(set *doblurav1alpha1.ReviewSet, ref forgeRef) string {
	if ref.Kind == "PullRequest" {
		return fmt.Sprintf("%s-pr-%d", set.Name, ref.Number)
	}
	clean := sanitiseLabel(ref.Ref)
	if len(clean) > 24 {
		clean = clean[:24]
	}
	return fmt.Sprintf("%s-%s-%s", set.Name, strings.Trim(clean, "-"), shortHash(ref.Ref))
}

func sanitiseLabel(s string) string {
	out := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9':
			out = append(out, c)
		case c >= 'A' && c <= 'Z':
			out = append(out, c+('a'-'A'))
		default:
			if len(out) > 0 && out[len(out)-1] != '-' {
				out = append(out, '-')
			}
		}
	}
	return string(out)
}

func shortHash(s string) string {
	sum := sha1.Sum([]byte(s)) //nolint:gosec // a short stable suffix, not a security boundary
	return hex.EncodeToString(sum[:])[:6]
}

func hasAnyLabel(have, want []string) bool {
	for _, h := range have {
		for _, w := range want {
			if strings.EqualFold(h, w) {
				return true
			}
		}
	}
	return false
}

// forgeToken reads the repository credential.
func (r *OdooReviewSetReconciler) forgeToken(
	ctx context.Context,
	set *doblurav1alpha1.ReviewSet,
) (string, error) {
	auth := set.Spec.Repository.Auth
	if auth == nil || auth.SecretRef == "" {
		// A public repository over the anonymous API. It works, and it is rate
		// limited by IP, which is fine for one repository and not for thirty.
		return "", nil
	}
	var sec corev1.Secret
	if err := r.Get(ctx, client.ObjectKey{
		Namespace: set.Namespace, Name: auth.SecretRef,
	}, &sec); err != nil {
		return "", fmt.Errorf("reading %q: %w", auth.SecretRef, err)
	}
	for _, key := range []string{"token", "password"} {
		if v := strings.TrimSpace(string(sec.Data[key])); v != "" {
			return v, nil
		}
	}
	return "", fmt.Errorf(
		"Secret %q holds no 'token' or 'password'. Asking a forge which pull "+
			"requests are open needs an API token; an SSH key cannot do it",
		auth.SecretRef)
}

func (r *OdooReviewSetReconciler) failSet(
	ctx context.Context,
	set *doblurav1alpha1.ReviewSet,
	st *doblurav1alpha1.ReviewSetStatus,
	reason string,
	cause error,
	every time.Duration,
) (ctrl.Result, error) {
	st.Message = cause.Error()
	meta.SetStatusCondition(&st.Conditions, metav1.Condition{
		Type: "Watching", Status: metav1.ConditionFalse, Reason: reason,
		Message: cause.Error(), ObservedGeneration: set.Generation,
	})
	// The existing environments are LEFT ALONE. A forge that is unreachable for
	// ten minutes is not a statement that every pull request has closed, and
	// deleting on a failed poll would tear down a room full of review
	// environments because somebody's token expired.
	return r.commitSet(ctx, set, st, ctrl.Result{RequeueAfter: every})
}

func (r *OdooReviewSetReconciler) commitSet(
	ctx context.Context,
	set *doblurav1alpha1.ReviewSet,
	st *doblurav1alpha1.ReviewSetStatus,
	res ctrl.Result,
) (ctrl.Result, error) {
	if equality.Semantic.DeepEqual(&set.Status, st) {
		return res, nil
	}
	set.Status = *st
	return res, r.Status().Update(ctx, set)
}

func (r *OdooReviewSetReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&doblurav1alpha1.ReviewSet{}).
		Owns(&doblurav1alpha1.OdooEnvironment{}).
		Named("reviewset").
		Complete(r)
}
