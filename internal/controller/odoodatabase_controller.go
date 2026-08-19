// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Antoni Romera

package controller

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	doblurav1alpha1 "github.com/doblura/doblura/api/v1alpha1"
)

// Choosing where a database lives, and saying whether a copy of it may be handed
// over.
//
// This controller did not exist. OdooDatabase was a kind with a wall of CEL
// guarding its multi-tenancy declaration, a status with placedOn and a
// HandoverSafe condition, printer columns for both, and a tested pure function —
// api/v1alpha1.Place — written to make the decision. Nothing called any of it.
//
// So an OdooDatabase could be created, would pass every rule, and then sit there
// for ever: empty status, `kubectl get` printing two blank columns, and nobody
// touching it. That reads as "doblura places databases on instances", which it did
// not. It is the same shape as the Ingress referring to middlewares nothing
// created and the chart offering to pin images nothing read — an unimplemented
// kind is just the largest version of it.
//
// The controller is small because everything it needs was already written and
// tested. That is the point: what was missing was the wiring, and wiring is
// exactly what nobody notices is absent.

// OdooDatabaseReconciler places databases and reports handover safety.
type OdooDatabaseReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=doblura.dev,resources=odoodatabases,verbs=get;list;watch
// +kubebuilder:rbac:groups=doblura.dev,resources=odoodatabases/status,verbs=get;update;patch

func (r *OdooDatabaseReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var db doblurav1alpha1.OdooDatabase
	if err := r.Get(ctx, req.NamespacedName, &db); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	st := db.Status.DeepCopy()
	st.ObservedGeneration = db.Generation

	r.place(ctx, &db, st)
	r.handover(&db, st)

	if equality.Semantic.DeepEqual(db.Status, *st) {
		// Nothing changed. Written out only when it differs, because a status
		// update is a write that wakes this controller again.
		return ctrl.Result{}, nil
	}
	db.Status = *st
	return ctrl.Result{}, r.Status().Update(ctx, &db)
}

// place chooses an instance, or records why none fits.
//
// Once placed, it STAYS placed. Re-running the choice on every reconcile would
// move a database to whichever instance had most room this minute — and the
// database itself does not move, so the status would simply start naming the
// wrong server. A placement is a decision, not an observation.
func (r *OdooDatabaseReconciler) place(
	ctx context.Context,
	db *doblurav1alpha1.OdooDatabase,
	st *doblurav1alpha1.OdooDatabaseStatus,
) {
	if st.PlacedOn != "" {
		return
	}

	var instances doblurav1alpha1.OdooInstanceList
	if err := r.List(ctx, &instances, client.InNamespace(db.Namespace)); err != nil {
		meta.SetStatusCondition(&st.Conditions, metav1.Condition{
			Type: doblurav1alpha1.ConditionPlaced, Status: metav1.ConditionFalse,
			Reason: "CannotList", ObservedGeneration: db.Generation,
			Message: "could not read the instances to choose from: " + err.Error(),
		})
		return
	}

	candidates := make([]doblurav1alpha1.PlacementCandidate, 0, len(instances.Items))
	for i := range instances.Items {
		in := &instances.Items[i]
		candidates = append(candidates, doblurav1alpha1.PlacementCandidate{
			Name: in.Name, Spec: in.Spec, Status: in.Status,
		})
	}

	chosen, err := doblurav1alpha1.Place(&db.Spec, candidates)
	if err != nil {
		// The refusal names every rejection and why, which is the whole reason
		// ErrNoInstance exists — "no instance available" on its own sends people
		// digging through YAML.
		st.Message = err.Error()
		meta.SetStatusCondition(&st.Conditions, metav1.Condition{
			Type: doblurav1alpha1.ConditionPlaced, Status: metav1.ConditionFalse,
			Reason: "NoInstance", Message: err.Error(), ObservedGeneration: db.Generation,
		})
		return
	}

	st.PlacedOn = chosen
	st.Message = ""
	meta.SetStatusCondition(&st.Conditions, metav1.Condition{
		Type: doblurav1alpha1.ConditionPlaced, Status: metav1.ConditionTrue,
		Reason: "Placed", ObservedGeneration: db.Generation,
		Message: fmt.Sprintf("on instance %s", chosen),
	})
}

// handover records whether a copy of this database may be given to a customer.
//
// Recorded for the SINGLE-TENANT case only when there is exactly one customer in
// it, because the question "safe for whom" has no answer otherwise: a shared
// database is safe to hand to nobody and the condition would have to be false
// without saying which customer it was false about. The reason carries that.
func (r *OdooDatabaseReconciler) handover(
	db *doblurav1alpha1.OdooDatabase,
	st *doblurav1alpha1.OdooDatabaseStatus,
) {
	tenants := map[string]bool{}
	for _, c := range db.Spec.Companies {
		tenants[c.TenantRef] = true
	}

	switch {
	case len(tenants) == 0:
		meta.SetStatusCondition(&st.Conditions, metav1.Condition{
			Type: doblurav1alpha1.ConditionHandoverSafe, Status: metav1.ConditionFalse,
			Reason: "Undeclared", ObservedGeneration: db.Generation,
			Message: "no companies are declared, so who is inside this database " +
				"is unknown. A copy of it cannot be given to anybody until it says",
		})
	case len(tenants) == 1:
		var only string
		for t := range tenants {
			only = t
		}
		ok, why := db.Spec.HandoverSafeFor(only)
		status := metav1.ConditionFalse
		reason := "NotSafe"
		if ok {
			status, reason = metav1.ConditionTrue, "Safe"
		}
		meta.SetStatusCondition(&st.Conditions, metav1.Condition{
			Type: doblurav1alpha1.ConditionHandoverSafe, Status: status,
			Reason: reason, ObservedGeneration: db.Generation,
			Message: fmt.Sprintf("for %s: %s", only, why),
		})
	default:
		// More than one customer in one database. Not safe for any of them, and
		// the message names them so nobody has to work out which.
		names := make([]string, 0, len(tenants))
		for t := range tenants {
			names = append(names, t)
		}
		sort.Strings(names)
		meta.SetStatusCondition(&st.Conditions, metav1.Condition{
			Type: doblurav1alpha1.ConditionHandoverSafe, Status: metav1.ConditionFalse,
			Reason: "MoreThanOneCustomer", ObservedGeneration: db.Generation,
			Message: "holds companies of " + strings.Join(names, ", ") +
				": a copy is one customer's data given to another",
		})
	}
}

func (r *OdooDatabaseReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&doblurav1alpha1.OdooDatabase{},
			builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		// An instance appearing is the event that unblocks a database nothing
		// could host. Without this, a database refused for want of an instance
		// stays refused until somebody edits it — which is the state that makes
		// people think placement does not work.
		Watches(
			&doblurav1alpha1.OdooInstance{},
			handler.EnqueueRequestsFromMapFunc(r.unplacedDatabases),
		).
		Named("odoodatabase").
		Complete(r)
}

// unplacedDatabases is every database in the namespace still waiting for one.
func (r *OdooDatabaseReconciler) unplacedDatabases(
	ctx context.Context,
	obj client.Object,
) []reconcile.Request {
	var list doblurav1alpha1.OdooDatabaseList
	if err := r.List(ctx, &list, client.InNamespace(obj.GetNamespace())); err != nil {
		return nil
	}
	var out []reconcile.Request
	for i := range list.Items {
		if list.Items[i].Status.PlacedOn != "" {
			continue
		}
		out = append(out, reconcile.Request{
			NamespacedName: client.ObjectKeyFromObject(&list.Items[i]),
		})
	}
	return out
}
