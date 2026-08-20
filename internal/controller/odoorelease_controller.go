// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Antoni Romera

package controller

import (
	"context"
	"fmt"
	"sort"
	"time"

	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	doblurav1alpha1 "github.com/doblura/doblura/api/v1alpha1"
)

// Rolling one version of somebody's product across their customers.
//
// The shape of this controller is three refusals and one small action. It moves a
// customer onto a release by putting the image in their catalogue and the version
// on their record; it never reaches into a running Production environment, for the
// reason written at the top of the CRD.

type OdooReleaseReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// releaseEntryName is the catalogue entry a release owns.
//
// Named after the RELEASE and not after the version, so a release that is
// re-pointed updates its own entry instead of leaving a trail of them — and so
// two releases cannot fight over one entry without somebody noticing they share
// a name.
func releaseEntryName(rel *doblurav1alpha1.OdooRelease) string {
	return "release-" + rel.Name
}

// Free-floating, with a blank line under it: RBAC markers are package-scoped and
// controller-gen reads them nowhere else.
//
// The `update` on odootenants is the only place in this operator where one
// customer's record is written by something other than a person, and it is worth
// seeing in the role rather than discovering in an audit.

// +kubebuilder:rbac:groups=doblura.dev,resources=odooreleases,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=doblura.dev,resources=odooreleases/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=doblura.dev,resources=odootenants,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=doblura.dev,resources=odoorehearsals,verbs=get;list;watch

func (r *OdooReleaseReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var rel doblurav1alpha1.OdooRelease
	if err := r.Get(ctx, req.NamespacedName, &rel); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	st := rel.Status.DeepCopy()
	st.ObservedGeneration = rel.Generation

	// What this release actually points at. Resolved once, recorded, and used for
	// every customer in the rollout: a rollout that spans a week and re-resolves a
	// tag each morning is a rollout where the customers disagree about what they
	// got.
	image, why := r.resolveImage(ctx, &rel)
	if image == "" {
		st.Phase, st.Message = doblurav1alpha1.ReleaseBlocked, why
		return r.save(ctx, &rel, st)
	}
	st.Image = image

	sel, err := metav1.LabelSelectorAsSelector(&rel.Spec.Selector)
	if err != nil {
		st.Phase = doblurav1alpha1.ReleaseBlocked
		st.Message = "the selector is not valid: " + err.Error()
		return r.save(ctx, &rel, st)
	}
	if sel.Empty() {
		// Belt and braces behind the CEL rule: an empty selector matches every
		// customer in the namespace, and this kind must never do that by accident.
		st.Phase = doblurav1alpha1.ReleaseBlocked
		st.Message = "an empty selector would select every customer in this namespace"
		return r.save(ctx, &rel, st)
	}

	var tenants doblurav1alpha1.OdooTenantList
	if err := r.List(ctx, &tenants, client.InNamespace(rel.Namespace),
		client.MatchingLabelsSelector{Selector: sel}); err != nil {
		return ctrl.Result{}, err
	}
	sort.Slice(tenants.Items, func(i, j int) bool {
		return tenants.Items[i].Name < tenants.Items[j].Name
	})

	now := time.Now()
	rolled, waiting, blocked := 0, 0, 0
	room := batchRoom(&rel, st, now)
	st.Customers = nil
	st.NextBatchAt = nil

	for i := range tenants.Items {
		t := &tenants.Items[i]
		row := doblurav1alpha1.ReleaseCustomer{Name: t.Name}

		switch {
		case t.Spec.ProductRelease == rel.Spec.Version:
			row.State = "onRelease"
			rolled++

		case !r.rehearsed(ctx, &rel, t, image):
			row.State = "notRehearsed"
			row.Why = fmt.Sprintf(
				"no rehearsal of %s's data against this image has succeeded. That is "+
					"the whole point of the gate: this operator exists so a migration "+
					"is something you have already watched happen", t.Name)
			waiting++
			blocked++

		case room <= 0:
			row.State = "waitingForBatch"
			row.Why = "the current batch is full or still soaking"
			waiting++

		default:
			if err := r.move(ctx, &rel, t, image); err != nil {
				row.State, row.Why = "blocked", err.Error()
				blocked++
				waiting++
				break
			}
			row.State = "onRelease"
			row.MovedAt = &metav1.Time{Time: now}
			rolled++
			room--
			st.LastMovedAt = &metav1.Time{Time: now}
		}
		st.Customers = append(st.Customers, row)
	}

	st.InScope = int32(len(tenants.Items))
	st.OnThisRelease = int32(rolled)
	st.Waiting = int32(waiting)

	switch {
	case len(tenants.Items) == 0:
		st.Phase = doblurav1alpha1.ReleasePending
		st.Message = "the selector matches no customer"
	case waiting == 0:
		st.Phase = doblurav1alpha1.ReleaseComplete
		st.Message = fmt.Sprintf("all %s are on %s", customers(rolled), rel.Spec.Version)
	case blocked > 0 && blocked == waiting:
		// Everything left is waiting on somebody, not on the clock. Saying
		// "soaking" here would promise that time alone finishes the rollout.
		st.Phase = doblurav1alpha1.ReleaseBlocked
		st.Message = fmt.Sprintf("%d of %s cannot move yet", waiting, customers(int(st.InScope)))
	default:
		st.Phase = doblurav1alpha1.ReleaseSoaking
		st.Message = fmt.Sprintf("%d of %d moved; the rest are waiting for the next batch",
			rolled, st.InScope)
		if st.LastMovedAt != nil {
			next := metav1.NewTime(st.LastMovedAt.Add(soakOf(&rel)))
			st.NextBatchAt = &next
		}
	}
	meta.SetStatusCondition(&st.Conditions, metav1.Condition{
		Type:   "Rolling",
		Status: boolCondition(st.Phase != doblurav1alpha1.ReleaseComplete),
		Reason: string(st.Phase), Message: st.Message,
		ObservedGeneration: rel.Generation,
	})

	res, err := r.save(ctx, &rel, st)
	if err != nil || st.Phase == doblurav1alpha1.ReleaseComplete {
		return res, err
	}
	// A deadline, so the next batch happens without anything else touching the
	// object. Capped, because a soak of a week must not depend on one timer that a
	// manager restart would lose.
	wake := soakOf(&rel)
	if wake > time.Hour {
		wake = time.Hour
	}
	return ctrl.Result{RequeueAfter: wake}, nil
}

// customers, because "all 1 customers are on 2026.3" is the kind of sentence that
// makes a person trust the rest of the screen a little less.
func customers(n int) string {
	if n == 1 {
		return "1 customer"
	}
	return fmt.Sprintf("%d customers", n)
}

func soakOf(rel *doblurav1alpha1.OdooRelease) time.Duration {
	if rel.Spec.Batch.Soak != nil {
		return rel.Spec.Batch.Soak.Duration
	}
	return 24 * time.Hour
}

// batchRoom is how many customers may move right now.
//
// Zero while a batch is soaking. The clock starts from the last customer moved
// rather than from a batch boundary, because "three at a time" and "and then wait
// a day" are the same rule stated twice, and the second is what people mean.
func batchRoom(rel *doblurav1alpha1.OdooRelease, st *doblurav1alpha1.OdooReleaseStatus, now time.Time) int {
	size := 3
	if rel.Spec.Batch.Size != nil {
		size = int(*rel.Spec.Batch.Size)
	}
	if st.LastMovedAt != nil && now.Sub(st.LastMovedAt.Time) < soakOf(rel) {
		return 0
	}
	return size
}

// rehearsed is whether this customer's own data has been through this image.
func (r *OdooReleaseReconciler) rehearsed(
	ctx context.Context,
	rel *doblurav1alpha1.OdooRelease,
	t *doblurav1alpha1.OdooTenant,
	image string,
) bool {
	if rel.Spec.RequireRehearsal != nil && !*rel.Spec.RequireRehearsal {
		return true // the acknowledgement is what the CEL rule made them write
	}
	var rehearsals doblurav1alpha1.OdooRehearsalList
	if err := r.List(ctx, &rehearsals, client.InNamespace(rel.Namespace)); err != nil {
		return false
	}
	for i := range rehearsals.Items {
		h := &rehearsals.Items[i]
		if h.Spec.ForTenant == t.Name && h.Spec.Image == image &&
			h.Status.Phase == doblurav1alpha1.PhaseSucceeded {
			return true
		}
	}
	return false
}

// move puts the release into a customer's catalogue and onto their record.
func (r *OdooReleaseReconciler) move(
	ctx context.Context,
	rel *doblurav1alpha1.OdooRelease,
	t *doblurav1alpha1.OdooTenant,
	image string,
) error {
	name := releaseEntryName(rel)
	updated := t.DeepCopy()

	found := false
	for i := range updated.Spec.Images {
		if updated.Spec.Images[i].Name != name {
			// Only one entry may be the default, and this release is taking it.
			updated.Spec.Images[i].Default = false
			continue
		}
		found = true
		updated.Spec.Images[i].Image = image
		updated.Spec.Images[i].FromBuild = ""
		updated.Spec.Images[i].Default = true
	}
	if !found {
		updated.Spec.Images = append(updated.Spec.Images, doblurav1alpha1.ImageCatalogueEntry{
			Name:        name,
			Image:       image,
			OdooVersion: t.Spec.OdooVersion,
			Default:     true,
			Notes:       "Put here by OdooRelease " + rel.Name + " (" + rel.Spec.Version + ")",
		})
	}
	updated.Spec.ProductRelease = rel.Spec.Version

	if equality.Semantic.DeepEqual(t.Spec, updated.Spec) {
		return nil
	}
	return r.Update(ctx, updated)
}

// resolveImage turns the release into the reference every customer will get.
func (r *OdooReleaseReconciler) resolveImage(
	ctx context.Context,
	rel *doblurav1alpha1.OdooRelease,
) (string, string) {
	if rel.Spec.Image != "" {
		return rel.Spec.Image, ""
	}
	var b doblurav1alpha1.OdooBuild
	err := r.Get(ctx, client.ObjectKey{Namespace: rel.Namespace, Name: rel.Spec.FromBuild}, &b)
	if err != nil {
		return "", fmt.Sprintf("this release is built by OdooBuild %q, and there is no "+
			"such build in this namespace", rel.Spec.FromBuild)
	}
	if b.Status.Image == "" {
		return "", fmt.Sprintf("OdooBuild %q has not produced an image yet (%s)",
			rel.Spec.FromBuild, b.Status.Phase)
	}
	return b.Status.Image, ""
}

func (r *OdooReleaseReconciler) save(
	ctx context.Context,
	rel *doblurav1alpha1.OdooRelease,
	st *doblurav1alpha1.OdooReleaseStatus,
) (ctrl.Result, error) {
	if equality.Semantic.DeepEqual(&rel.Status, st) {
		return ctrl.Result{}, nil
	}
	rel.Status = *st
	return ctrl.Result{}, r.Status().Update(ctx, rel)
}

func (r *OdooReleaseReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&doblurav1alpha1.OdooRelease{}).
		Named("odoorelease").
		Complete(r)
}
