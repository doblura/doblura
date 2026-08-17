// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Antoni Romera

package controller

import (
	"context"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	doblurav1alpha1 "github.com/doblura/doblura/api/v1alpha1"
)

// OdooInstanceReconciler observes Postgres servers.
//
// This controller exists because three fields did not work. `status.serverVersion`
// was documented as the thing that warns you about a pg18 client against a pg16
// server — the failure that cost twenty minutes on the first real run and surfaces
// as a bare "Couldn't restore database". `status.available` was printed by
// `kubectl get`. And `capacity.reservedGi` was documented as "Placement refuses an
// instance whose free space would drop below it".
//
// None of them was ever written or read, because nothing observed an instance.
// Which made the last one the most dangerous kind of field: an operator sets it,
// reads it back on the customer record, and believes the cluster is bounded.
//
// The shape is RunboatLink's, deliberately, because the problem is the same: poll
// something outside the cluster on a timer, and be honest about staleness. Two
// rules carried over from it —
//
//   - A failed probe NEVER clears a previous observation. "The disk is full" and
//     "we could not measure the disk" are different facts, and the second must not
//     be rendered as the first.
//   - The status is compared before it is written, because a controller that runs
//     on a timer writes on every tick otherwise, and every write wakes the watch.
type OdooInstanceReconciler struct {
	client.Client
	Scheme *runtime.Scheme

	// ProbeImage needs psql, sh, df and awk. Passed as a flag rather than read
	// from the chart's -defaults ConfigMap, because nothing reads that ConfigMap:
	// see the note in values.yaml. A knob that does nothing is worse than no knob,
	// so this one is wired end to end or not offered.
	ProbeImage string
}

// +kubebuilder:rbac:groups=doblura.dev,resources=odooinstances,verbs=get;list;watch
// +kubebuilder:rbac:groups=doblura.dev,resources=odooinstances/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=doblura.dev,resources=odoodatabases,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch;create;delete

func (r *OdooInstanceReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	lg := log.FromContext(ctx)

	var inst doblurav1alpha1.OdooInstance
	if err := r.Get(ctx, req.NamespacedName, &inst); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	st := inst.Status.DeepCopy()
	st.ObservedGeneration = inst.Generation
	every := inst.Spec.ObserveEvery().Duration

	// How many databases Doblura has placed here. This is cluster state, not
	// server state, so it does not need the probe and is refreshed every pass.
	placed, err := r.countPlaced(ctx, inst.Namespace, inst.Name)
	if err != nil {
		return ctrl.Result{}, err
	}
	st.Databases = placed
	st.Available = inst.Spec.Capacity.MaxDatabases - placed
	if st.Available < 0 {
		st.Available = 0
	}

	// ── Is the observation still current? ──
	if st.LastProbe != nil {
		if age := time.Since(st.LastProbe.Time); age < every {
			r.setSchedulable(&inst, st)
			return r.finish(ctx, &inst, st, every-age)
		}
	}

	// ── Drive the probe Pod ──
	pod := &corev1.Pod{}
	key := types.NamespacedName{Namespace: inst.Namespace, Name: probePodName(inst.Name)}
	switch err := r.Get(ctx, key, pod); {
	case apierrors.IsNotFound(err):
		want := instanceProbePod(&inst, r.ProbeImage)
		if err := ctrl.SetControllerReference(&inst, want, r.Scheme); err != nil {
			return ctrl.Result{}, err
		}
		if err := r.Create(ctx, want); err != nil && !apierrors.IsAlreadyExists(err) {
			return ctrl.Result{}, err
		}
		// Probes finish in seconds. Coming back sooner than the observe interval
		// is the whole point of this branch.
		return r.finish(ctx, &inst, st, 5*time.Second)

	case err != nil:
		return ctrl.Result{}, err
	}

	switch pod.Status.Phase {
	case corev1.PodPending, corev1.PodRunning:
		// A probe that never finishes must not wedge the instance forever. Two
		// minutes is generous for `SHOW server_version` and long enough that a
		// slow image pull is not mistaken for an unreachable server.
		if age := time.Since(pod.CreationTimestamp.Time); age > 2*time.Minute {
			r.observationFailed(st, fmt.Sprintf(
				"the probe Pod has not finished after %s (image pull, or the server is not answering)",
				age.Round(time.Second)))
			_ = r.Delete(ctx, pod)
			return r.finish(ctx, &inst, st, every)
		}
		return r.finish(ctx, &inst, st, 5*time.Second)

	case corev1.PodSucceeded, corev1.PodFailed:
		msg := terminationMessage(pod)
		res, perr := parseProbeResult(msg)
		if perr != nil {
			r.observationFailed(st, perr.Error())
			lg.Info("instance probe failed, keeping the previous observation",
				"instance", inst.Name, "error", perr.Error())
		} else {
			applyProbe(st, res)
			meta.SetStatusCondition(&st.Conditions, metav1.Condition{
				Type: doblurav1alpha1.ConditionReachable, Status: metav1.ConditionTrue,
				Reason: "Probed", Message: "server_version " + res.ServerVersion,
			})
			now := metav1.Now()
			st.LastProbe = &now
			if res.Error != "" {
				// Connected, but could not answer everything. Reported rather than
				// hidden: the usual cause is that free space needs privileges a
				// CREATEDB-only user does not have, and somebody has to know that
				// before they trust capacity.reservedGi.
				st.Message = res.Error
			} else {
				st.Message = ""
			}
		}
		// The Pod has served its purpose either way. Deleted rather than left with
		// a TTL, because its env references the admin password Secret and a
		// finished Pod holding that reference is a thing to explain.
		_ = r.Delete(ctx, pod)
	}

	r.setSchedulable(&inst, st)
	return r.finish(ctx, &inst, st, every)
}

// applyProbe copies an observation into status, field by field.
//
// A field the probe could not answer is left as it was rather than zeroed. Zeroing
// free space would read as "full" to placement, and zeroing the server version
// would silently remove the version warning.
func applyProbe(st *doblurav1alpha1.OdooInstanceStatus, res *probeResult) {
	if res.ServerVersion != "" {
		st.ServerVersion = res.ServerVersion
	}
	if g := gib(res.DiskTotalBytes); g != nil {
		st.DiskTotalGi = g
	}
	if g := gib(res.DiskFreeBytes); g != nil {
		st.DiskFreeGi = g
	}
}

// observationFailed records that the measurement failed, without discarding it.
func (r *OdooInstanceReconciler) observationFailed(st *doblurav1alpha1.OdooInstanceStatus, why string) {
	meta.SetStatusCondition(&st.Conditions, metav1.Condition{
		Type: doblurav1alpha1.ConditionReachable, Status: metav1.ConditionFalse,
		Reason: "ProbeFailed", Message: why,
	})
	st.Message = why
}

// setSchedulable answers the question placement will ask, in advance.
//
// It is a condition rather than a computed field because the reason matters as
// much as the answer: "cordoned", "at capacity" and "the disk was never observed"
// are three different things to do something about.
func (r *OdooInstanceReconciler) setSchedulable(
	inst *doblurav1alpha1.OdooInstance, st *doblurav1alpha1.OdooInstanceStatus,
) {
	cond := metav1.Condition{
		Type: doblurav1alpha1.ConditionSchedulable, Status: metav1.ConditionTrue,
		Reason: "Available", Message: fmt.Sprintf("%d of %d databases placed",
			st.Databases, inst.Spec.Capacity.MaxDatabases),
	}
	switch {
	case inst.Spec.Unschedulable != nil && *inst.Spec.Unschedulable:
		cond.Status, cond.Reason, cond.Message =
			metav1.ConditionFalse, "Cordoned", "spec.unschedulable is set; nothing new lands here"
	case !meta.IsStatusConditionTrue(st.Conditions, doblurav1alpha1.ConditionReachable):
		cond.Status, cond.Reason, cond.Message =
			metav1.ConditionFalse, "Unreachable", "the last probe did not succeed"
	case st.Available <= 0:
		cond.Status, cond.Reason = metav1.ConditionFalse, "AtCapacity"
	default:
		// Ask the same function placement asks, so the condition and the placer
		// can never disagree about headroom.
		probe := doblurav1alpha1.OdooInstance{Spec: inst.Spec, Status: *st}
		if ok, why := probe.HeadroomFor(nil); !ok {
			cond.Status, cond.Reason, cond.Message = metav1.ConditionFalse, "NoHeadroom", why
		}
	}
	meta.SetStatusCondition(&st.Conditions, cond)
}

// countPlaced counts the OdooDatabases assigned to this instance.
//
// Counts spec.instanceRef and status.placedOn: the first is what a human asked
// for, the second is what the placer chose, and a database is on the server either
// way. Counting only one of them would undercount and let the ceiling be passed.
func (r *OdooInstanceReconciler) countPlaced(ctx context.Context, ns, instance string) (int32, error) {
	var dbs doblurav1alpha1.OdooDatabaseList
	if err := r.List(ctx, &dbs, client.InNamespace(ns)); err != nil {
		return 0, err
	}
	var n int32
	for i := range dbs.Items {
		d := &dbs.Items[i]
		if d.Spec.InstanceRef == instance || d.Status.PlacedOn == instance {
			n++
		}
	}
	return n, nil
}

func (r *OdooInstanceReconciler) finish(
	ctx context.Context,
	inst *doblurav1alpha1.OdooInstance,
	st *doblurav1alpha1.OdooInstanceStatus,
	requeue time.Duration,
) (ctrl.Result, error) {
	if !equality.Semantic.DeepEqual(&inst.Status, st) {
		inst.Status = *st
		if err := r.Status().Update(ctx, inst); err != nil {
			return ctrl.Result{}, err
		}
	}
	if requeue < time.Second {
		requeue = time.Second
	}
	return ctrl.Result{RequeueAfter: requeue}, nil
}

// terminationMessage returns what the probe container reported.
func terminationMessage(pod *corev1.Pod) string {
	for _, cs := range pod.Status.ContainerStatuses {
		if cs.Name != "probe" {
			continue
		}
		if t := cs.State.Terminated; t != nil {
			return t.Message
		}
		if t := cs.LastTerminationState.Terminated; t != nil {
			return t.Message
		}
	}
	return ""
}

func (r *OdooInstanceReconciler) SetupWithManager(mgr ctrl.Manager) error {
	// No GenerationChangedPredicate, for the same reason as RunboatLink: this
	// controller drives itself with RequeueAfter and writes status on most passes.
	// The write-loop defence is the DeepEqual in finish().
	return ctrl.NewControllerManagedBy(mgr).
		For(&doblurav1alpha1.OdooInstance{}).
		Owns(&corev1.Pod{}).
		Named("odooinstance").
		Complete(r)
}
