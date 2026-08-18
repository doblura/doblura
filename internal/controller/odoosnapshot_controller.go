// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Antoni Romera

package controller

import (
	"context"
	"fmt"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	doblurav1alpha1 "github.com/doblura/doblura/api/v1alpha1"
)

// OdooSnapshotReconciler produces anonymized dumps.
//
// Unlike a rehearsal, here the whole pipeline shares one work database and one
// scratch volume, so it runs as ONE Job with chained init containers. If any of
// them fails, the Job fails and the work database is dropped: no half-anonymized
// copy of production is left lying around.
type OdooSnapshotReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=doblura.dev,resources=odoosnapshots,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=doblura.dev,resources=odoosnapshots/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=batch,resources=cronjobs,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=networking.k8s.io,resources=networkpolicies,verbs=get;list;watch;create;update;patch;delete

func (r *OdooSnapshotReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var snap doblurav1alpha1.OdooSnapshot
	if err := r.Get(ctx, req.NamespacedName, &snap); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	st := snap.Status.DeepCopy()
	st.ObservedGeneration = snap.Generation

	// The pipeline configuration, in an auditable ConfigMap: which tables get
	// emptied and which columns get masked is written down and reviewable without
	// reading the operator's code. For a copy holding personal data that is not
	// convenience, it is evidence of diligence.
	if err := r.ensureConfig(ctx, &snap); err != nil {
		return ctrl.Result{}, err
	}

	// The pod holds a COMPLETE un-anonymized copy for minutes. It is the most
	// sensitive pod in the cluster and it has no reason to reach anything.
	if snap.Spec.DenyEgress == nil || *snap.Spec.DenyEgress {
		if err := r.ensureNetworkPolicy(ctx, &snap); err != nil {
			return ctrl.Result{}, err
		}
	}

	if snap.Spec.Schedule != "" {
		if err := r.ensureCronJob(ctx, &snap); err != nil {
			return ctrl.Result{}, err
		}
		st.Phase = doblurav1alpha1.SnapPending
		st.Message = "scheduled: " + snap.Spec.Schedule
		meta.SetStatusCondition(&st.Conditions, metav1.Condition{
			Type: "Scheduled", Status: metav1.ConditionTrue,
			Reason: "CronJobCreated", Message: "CronJob " + snap.Name + " created",
			ObservedGeneration: snap.Generation,
		})
	}

	st.TablesTruncated = int32(len(snap.Spec.TablesToTruncate()))
	st.ColumnsMasked = int32(len(snap.Spec.RulesToApply()))

	if equality.Semantic.DeepEqual(&snap.Status, st) {
		return ctrl.Result{}, nil
	}
	snap.Status = *st
	return ctrl.Result{}, r.Status().Update(ctx, &snap)
}

func (r *OdooSnapshotReconciler) ensureConfig(ctx context.Context, snap *doblurav1alpha1.OdooSnapshot) error {
	work := workDBName(snap)
	data := map[string]string{
		"odoo.conf": snapshotOdooConf(snap, work),
	}
	switch snap.Spec.Mask.Engine {
	case doblurav1alpha1.EngineSQL:
		data["mask.sh"] = sqlMaskScript(&snap.Spec, work)
	case doblurav1alpha1.EngineCustom:
		// The configuration comes with the user's own image.
	default:
		data["greenmask.yaml"] = greenmaskConfig(&snap.Spec, work)
	}

	cm := &corev1.ConfigMap{
		TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "ConfigMap"},
		ObjectMeta: metav1.ObjectMeta{Name: snap.Name + "-pipeline", Namespace: snap.Namespace},
		Data:       data,
	}
	if err := ctrl.SetControllerReference(snap, cm, r.Scheme); err != nil {
		return err
	}
	return r.Patch(ctx, cm, client.Apply, fieldOwner, client.ForceOwnership)
}

// ensureNetworkPolicy fences the pod in.
//
// It can only talk to DNS and to Postgres. No internet, no other cluster
// services. If somebody compromises this pod, what they hold is a copy of
// production; the goal is that they cannot get it out of here.
func (r *OdooSnapshotReconciler) ensureNetworkPolicy(ctx context.Context, snap *doblurav1alpha1.OdooSnapshot) error {
	tcp := corev1.ProtocolTCP
	udp := corev1.ProtocolUDP
	// Both configured ports, not 5432 twice. A snapshot talks to two servers —
	// the source it reads and the work database it rebuilds in — and either can
	// be somewhere other than the default. Hardcoding the port turned that into
	// a hang with no explanation anywhere near the cause.
	dns := intstrFromInt(53)
	ports := []networkingv1.NetworkPolicyPort{}
	seen := map[int32]bool{}
	for _, p := range []int32{
		orDefaultInt32(snap.Spec.Source.Port, 5432),
		orDefaultInt32(snap.Spec.Work.Port, 5432),
	} {
		if seen[p] {
			continue
		}
		seen[p] = true
		port := intstrFromInt(p)
		ports = append(ports, networkingv1.NetworkPolicyPort{Protocol: &tcp, Port: &port})
	}

	np := &networkingv1.NetworkPolicy{
		TypeMeta:   metav1.TypeMeta{APIVersion: "networking.k8s.io/v1", Kind: "NetworkPolicy"},
		ObjectMeta: metav1.ObjectMeta{Name: snap.Name + "-deny-egress", Namespace: snap.Namespace},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{
				MatchLabels: map[string]string{"doblura.dev/snapshot": snap.Name},
			},
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeEgress},
			Egress: []networkingv1.NetworkPolicyEgressRule{
				{Ports: []networkingv1.NetworkPolicyPort{
					{Protocol: &udp, Port: &dns},
					{Protocol: &tcp, Port: &dns},
				}},
				{Ports: ports},
			},
		},
	}
	if err := ctrl.SetControllerReference(snap, np, r.Scheme); err != nil {
		return err
	}
	return r.Patch(ctx, np, client.Apply, fieldOwner, client.ForceOwnership)
}

func (r *OdooSnapshotReconciler) ensureCronJob(ctx context.Context, snap *doblurav1alpha1.OdooSnapshot) error {
	cj := &batchv1.CronJob{
		TypeMeta:   metav1.TypeMeta{APIVersion: "batch/v1", Kind: "CronJob"},
		ObjectMeta: metav1.ObjectMeta{Name: snap.Name, Namespace: snap.Namespace},
		Spec: batchv1.CronJobSpec{
			Schedule: snap.Spec.Schedule,
			// Forbid: two anonymization runs against the same work database
			// would collide. And if one takes longer than the interval, what you
			// want is to skip the next, not to queue them up.
			ConcurrencyPolicy:          batchv1.ForbidConcurrent,
			SuccessfulJobsHistoryLimit: ptr(int32(3)),
			FailedJobsHistoryLimit:     ptr(int32(3)),
			JobTemplate: batchv1.JobTemplateSpec{
				Spec: batchv1.JobSpec{
					BackoffLimit: ptr(int32(0)),
					Template:     snapshotPodTemplate(snap),
				},
			},
		},
	}
	if err := ctrl.SetControllerReference(snap, cj, r.Scheme); err != nil {
		return err
	}
	return r.Patch(ctx, cj, client.Apply, fieldOwner, client.ForceOwnership)
}

func (r *OdooSnapshotReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&doblurav1alpha1.OdooSnapshot{}, builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		Owns(&batchv1.CronJob{}).
		Named("odoosnapshot").
		Complete(r)
}

func workDBName(snap *doblurav1alpha1.OdooSnapshot) string {
	return "anon_" + snap.Name
}

func snapshotOdooConf(snap *doblurav1alpha1.OdooSnapshot, work string) string {
	return fmt.Sprintf(`[options]
db_host = %s
db_port = %d
db_user = %s
db_name = %s
list_db = False
`, snap.Spec.Work.Host, orDefaultInt32(snap.Spec.Work.Port, 5432), snap.Spec.Work.User, work)
}
