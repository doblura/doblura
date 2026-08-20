// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Antoni Romera

package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
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
	} else {
		// No schedule: take one, once.
		//
		// Without this the object was inert and SILENT. A snapshot with no
		// schedule produced a ConfigMap, a NetworkPolicy, a status with no phase
		// and no message, and nothing that would ever run — so somebody asking for
		// an anonymised copy got an object that looked accepted and did nothing,
		// with no sentence anywhere saying why. It is the shape of defect this
		// project keeps finding in itself, and it was in the one kind whose job is
		// to produce the data everything else is tested against.
		//
		// Once, and never again: the Job is created if it is absent and is not
		// recreated when it finishes. Re-running would read production a second
		// time, unasked, and this is the most sensitive pod in the cluster.
		if err := r.ensureJob(ctx, &snap, st); err != nil {
			return ctrl.Result{}, err
		}
	}

	st.TablesTruncated = int32(len(snap.Spec.TablesToTruncate()))
	st.ColumnsDeclared = int32(len(snap.Spec.RulesToApply()))

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

// ensureJob is the one-off run, and the status that follows it.
func (r *OdooSnapshotReconciler) ensureJob(
	ctx context.Context,
	snap *doblurav1alpha1.OdooSnapshot,
	st *doblurav1alpha1.OdooSnapshotStatus,
) error {
	name := snap.Name + "-run"

	var job batchv1.Job
	err := r.Get(ctx, client.ObjectKey{Namespace: snap.Namespace, Name: name}, &job)
	switch {
	case apierrors.IsNotFound(err):
		job = batchv1.Job{
			TypeMeta:   metav1.TypeMeta{APIVersion: "batch/v1", Kind: "Job"},
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: snap.Namespace},
			Spec: batchv1.JobSpec{
				// Zero retries, as for the CronJob: a second attempt is a second
				// read of production, and whatever made the first one fail is
				// still true.
				BackoffLimit: ptr(int32(0)),
				Template:     snapshotPodTemplate(snap),
			},
		}
		if err := ctrl.SetControllerReference(snap, &job, r.Scheme); err != nil {
			return err
		}
		if err := r.Create(ctx, &job); err != nil && !apierrors.IsAlreadyExists(err) {
			return err
		}
		st.Phase = doblurav1alpha1.SnapCopying
		st.Message = "taking a copy; no schedule was set, so this happens once"
		return nil
	case err != nil:
		return err
	}

	switch {
	case job.Status.Succeeded > 0:
		st.Phase = doblurav1alpha1.SnapSucceeded
		st.Message = "copy taken"
		if st.LastSuccessfulTime == nil && job.Status.CompletionTime != nil {
			st.LastSuccessfulTime = job.Status.CompletionTime
		}
		// What the run actually did, read back off the pod. Without this the
		// object reports the count of rules the spec asks for and calls it
		// evidence.
		r.applyMaskReport(ctx, snap, st, name)
		meta.SetStatusCondition(&st.Conditions, metav1.Condition{
			Type: "Taken", Status: metav1.ConditionTrue, Reason: "JobCompleted",
			Message: "Job " + name + " completed", ObservedGeneration: snap.Generation,
		})
	case job.Status.Failed > 0:
		st.Phase = doblurav1alpha1.SnapFailed
		// Named, because the reason is in the pod and not in this object, and
		// "failed" with nothing to open is a sentence nobody can act on.
		st.Message = "the copy failed; check the logs of Job " + name
		meta.SetStatusCondition(&st.Conditions, metav1.Condition{
			Type: "Taken", Status: metav1.ConditionFalse, Reason: "JobFailed",
			Message: "Job " + name + " failed", ObservedGeneration: snap.Generation,
		})
	default:
		st.Phase = doblurav1alpha1.SnapCopying
		st.Message = "taking a copy; no schedule was set, so this happens once"
	}
	return nil
}

// maskReport is what the dump step leaves in its termination message.
type maskReport struct {
	Tables  []string `json:"tables"`
	Columns []string `json:"columns"`
	Masked  int      `json:"masked"`
	// SizeBytes of the dump. The field it fills has a printcolumn of its own and
	// nothing had ever written it, so `kubectl get odoosnapshots` had a Size
	// column that was always empty.
	SizeBytes int64 `json:"size_bytes"`
}

// applyMaskReport reads it back onto the object.
//
// Best effort on purpose: a snapshot that ran and produced a dump has succeeded
// whether or not its report could be read, and refusing to record the success
// because the accounting is missing would be the worse failure. When it cannot be
// read, the counts stay at zero rather than being filled in from the spec — an
// unknown is shown as unknown.
func (r *OdooSnapshotReconciler) applyMaskReport(
	ctx context.Context,
	snap *doblurav1alpha1.OdooSnapshot,
	st *doblurav1alpha1.OdooSnapshotStatus,
	jobName string,
) {
	var pods corev1.PodList
	if err := r.List(ctx, &pods, client.InNamespace(snap.Namespace),
		client.MatchingLabels{"job-name": jobName}); err != nil {
		return
	}
	for i := range pods.Items {
		// The init container statuses, not the containers': the report comes from
		// 4-dump, and Kubernetes does not copy a Job's labels onto its pods but it
		// does keep every init container's termination message here.
		for _, cs := range pods.Items[i].Status.InitContainerStatuses {
			if cs.Name != "4-dump" || cs.State.Terminated == nil {
				continue
			}
			var rep maskReport
			if err := json.Unmarshal([]byte(cs.State.Terminated.Message), &rep); err != nil {
				continue
			}
			st.ColumnsMasked = int32(rep.Masked)
			st.SizeBytes = rep.SizeBytes
			st.NotMasked = nil
			st.NotMasked = append(st.NotMasked, rep.Tables...)
			st.NotMasked = append(st.NotMasked, rep.Columns...)
			sort.Strings(st.NotMasked)
			return
		}
	}
}

func (r *OdooSnapshotReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&doblurav1alpha1.OdooSnapshot{}, builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		Owns(&batchv1.CronJob{}).
		// Without this the one-off Job would finish and the object would go on
		// saying "taking a copy" until something else happened to wake the
		// reconciler — and GenerationChangedPredicate above means a status change
		// is not something else.
		Owns(&batchv1.Job{}).
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
