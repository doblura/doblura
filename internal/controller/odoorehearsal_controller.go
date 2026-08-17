// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Antoni Romera

// Package controller implementa el reconciler de OdooRehearsal.
package controller

import (
	"context"
	"fmt"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	doblurav1alpha1 "github.com/doblura/doblura/api/v1alpha1"
)

const fieldOwner = client.FieldOwner("doblura")

// OdooRehearsalReconciler reconciles OdooRehearsal.
//
// A rehearsal is a stateful sequence, not a deployment: restore, migrate,
// assert, clean up. Each step is a Job and the controller advances based on the
// previous one's outcome. That is why this is a controller and not a Crossplane
// Composition: it has to observe real state, wait, and decide.
type OdooRehearsalReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=doblura.dev,resources=odoorehearsals,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=doblura.dev,resources=odoorehearsals/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=batch,resources=jobs,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=core,resources=pods,verbs=get;list;watch
// +kubebuilder:rbac:groups=core,resources=configmaps,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=core,resources=secrets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=core,resources=events,verbs=create;patch

// Reconcile advances the rehearsal by one step.
func (r *OdooRehearsalReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	l := log.FromContext(ctx)

	var reh doblurav1alpha1.OdooRehearsal
	if err := r.Get(ctx, req.NamespacedName, &reh); err != nil {
		// IgnoreNotFound: otherwise a deleted object is retried forever.
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// Terminal: a rehearsal is never re-run. It is the historical record of one
	// migration against one set of data; re-running it on the same object would
	// destroy the result somebody wants to read. To repeat it, create another
	// object.
	if reh.Status.Phase == doblurav1alpha1.PhaseSucceeded ||
		reh.Status.Phase == doblurav1alpha1.PhaseFailed {
		return ctrl.Result{}, nil
	}

	// We work on a copy of the status and compare at the end. This is the first
	// of the three defences against the hot loop.
	st := reh.Status.DeepCopy()
	st.ObservedGeneration = reh.Generation

	if st.StartedAt == nil {
		now := metav1.Now()
		st.StartedAt = &now
		st.DatabaseName = fmt.Sprintf("rehearsal-%s-%d", reh.Name, now.Unix())
		st.Phase = doblurav1alpha1.PhaseRestoring
		l.Info("rehearsal started", "database", st.DatabaseName)
		return r.finish(ctx, &reh, st, ctrl.Result{Requeue: true})
	}

	// Total-time guard. This one really is a hygiene timeout, distinct from the
	// migration budget.
	if hard := r.hardTimeout(&reh); time.Since(st.StartedAt.Time) > hard {
		r.fail(st, fmt.Sprintf("the rehearsal exceeded its hardTimeout of %s", hard))
		return r.finish(ctx, &reh, st, ctrl.Result{})
	}

	// GitHub App installation tokens expire after an hour: they are minted right
	// before each Job so the init container always has a fresh one.
	if err := r.ensureGitHubAppTokens(ctx, &reh); err != nil {
		// Return the error rather than moving to Failed: this is a configuration
		// or network problem, it is recoverable, and exponential backoff is the
		// right response.
		return ctrl.Result{}, err
	}

	job, err := r.ensureJob(ctx, &reh, st)
	if err != nil {
		return ctrl.Result{}, err
	}

	switch {
	case job.Status.Succeeded > 0:
		was := r.advance(&reh, st, job)

		// Requeue whenever the phase moved, and this is not optional.
		//
		// Advancing only writes the status; the Job for the NEXT phase is created
		// on the following pass. Nothing else will trigger that pass:
		// GenerationChangedPredicate deliberately filters out our own status
		// writes, and the Job that woke us has already reached its final state,
		// so no further event is coming. Without this requeue the rehearsal
		// stalls forever one phase short — which is exactly what the first real
		// end-to-end run did, sitting in Asserting with no assert Job.
		if st.Phase != was && !terminal(st.Phase) {
			return r.finish(ctx, &reh, st, ctrl.Result{Requeue: true})
		}
	case job.Status.Failed > 0:
		r.fail(st, fmt.Sprintf("phase %s failed; check the logs of Job %s", st.Phase, job.Name))
	default:
		// In progress. No RequeueAfter: Owns(&batchv1.Job{}) wakes us when the
		// Job changes. Polling would be work for nothing.
		return r.finish(ctx, &reh, st, ctrl.Result{})
	}

	return r.finish(ctx, &reh, st, ctrl.Result{})
}

// advance moves the rehearsal to the next phase and records the results.
//
// It returns the phase it left behind, so Reconcile can tell whether anything
// moved. That matters more than it looks: see the requeue comment below.
func (r *OdooRehearsalReconciler) advance(
	reh *doblurav1alpha1.OdooRehearsal,
	st *doblurav1alpha1.OdooRehearsalStatus,
	job *batchv1.Job,
) doblurav1alpha1.RehearsalPhase {
	was := st.Phase
	switch st.Phase {
	case doblurav1alpha1.PhaseRestoring:
		meta.SetStatusCondition(&st.Conditions, metav1.Condition{
			Type:               doblurav1alpha1.ConditionRestored,
			Status:             metav1.ConditionTrue,
			Reason:             "SnapshotRestored",
			Message:            "snapshot restored and neutralized",
			ObservedGeneration: reh.Generation,
		})
		st.Phase = doblurav1alpha1.PhaseMigrating

	case doblurav1alpha1.PhaseMigrating:
		// The migration duration is the result people come here for.
		d := jobDuration(job)
		st.UpgradeDuration = &metav1.Duration{Duration: d}

		meta.SetStatusCondition(&st.Conditions, metav1.Condition{
			Type:               doblurav1alpha1.ConditionMigrated,
			Status:             metav1.ConditionTrue,
			Reason:             "UpgradeCompleted",
			Message:            fmt.Sprintf("the migration finished in %s", d.Truncate(time.Second)),
			ObservedGeneration: reh.Generation,
		})

		// The budget is evaluated as a separate condition: the migration may have
		// finished cleanly AND still not fit the window. Those are two distinct
		// facts and the user needs to tell them apart.
		if b := reh.Spec.Budget; b != nil && b.MaxUpgradeDuration != nil {
			within := d <= b.MaxUpgradeDuration.Duration
			cond := metav1.Condition{
				Type:               doblurav1alpha1.ConditionWithinBudget,
				Status:             metav1.ConditionTrue,
				Reason:             "WithinWindow",
				Message:            fmt.Sprintf("%s, within the budget of %s", d.Truncate(time.Second), b.MaxUpgradeDuration.Duration),
				ObservedGeneration: reh.Generation,
			}
			if !within {
				cond.Status = metav1.ConditionFalse
				cond.Reason = "BudgetExceeded"
				cond.Message = fmt.Sprintf(
					"the migration took %s, over the budget of %s: this release does not fit the maintenance window",
					d.Truncate(time.Second), b.MaxUpgradeDuration.Duration)
			}
			meta.SetStatusCondition(&st.Conditions, cond)

			if !within {
				r.fail(st, cond.Message)
				return was
			}
		}
		st.Phase = doblurav1alpha1.PhaseAsserting

	case doblurav1alpha1.PhaseAsserting:
		meta.SetStatusCondition(&st.Conditions, metav1.Condition{
			Type:               doblurav1alpha1.ConditionAsserted,
			Status:             metav1.ConditionTrue,
			Reason:             "AssertionsPassed",
			Message:            "critical models are still queryable",
			ObservedGeneration: reh.Generation,
		})
		st.Phase = doblurav1alpha1.PhaseSucceeded
		now := metav1.Now()
		st.FinishedAt = &now
		st.Message = "rehearsal passed"
		if st.UpgradeDuration != nil {
			st.Message = fmt.Sprintf("rehearsal passed; the migration took %s",
				st.UpgradeDuration.Duration.Truncate(time.Second))
		}
		meta.SetStatusCondition(&st.Conditions, metav1.Condition{
			Type:               doblurav1alpha1.ConditionSucceeded,
			Status:             metav1.ConditionTrue,
			Reason:             "RehearsalPassed",
			Message:            st.Message,
			ObservedGeneration: reh.Generation,
		})
	}
	return was
}

// terminal reports whether a phase is an end state.
func terminal(p doblurav1alpha1.RehearsalPhase) bool {
	return p == doblurav1alpha1.PhaseSucceeded || p == doblurav1alpha1.PhaseFailed
}

func (r *OdooRehearsalReconciler) fail(st *doblurav1alpha1.OdooRehearsalStatus, msg string) {
	st.Phase = doblurav1alpha1.PhaseFailed
	st.Message = msg
	now := metav1.Now()
	st.FinishedAt = &now
	meta.SetStatusCondition(&st.Conditions, metav1.Condition{
		Type:    doblurav1alpha1.ConditionSucceeded,
		Status:  metav1.ConditionFalse,
		Reason:  "RehearsalFailed",
		Message: msg,
	})
}

// finish writes the status only when it genuinely changed.
func (r *OdooRehearsalReconciler) finish(
	ctx context.Context,
	reh *doblurav1alpha1.OdooRehearsal,
	st *doblurav1alpha1.OdooRehearsalStatus,
	res ctrl.Result,
) (ctrl.Result, error) {
	// equality.Semantic, not reflect.DeepEqual: it treats nil and an empty slice
	// as equal, which is what the Kubernetes API considers equivalent. With
	// reflect.DeepEqual you would write the status on every pass and get the
	// classic hot loop.
	if equality.Semantic.DeepEqual(&reh.Status, st) {
		return res, nil
	}
	reh.Status = *st
	// Status().Update, not Update: the spec is not ours to write.
	return res, r.Status().Update(ctx, reh)
}

// ensureJob server-side-applies the current phase's Job, or returns the existing one.
func (r *OdooRehearsalReconciler) ensureJob(
	ctx context.Context,
	reh *doblurav1alpha1.OdooRehearsal,
	st *doblurav1alpha1.OdooRehearsalStatus,
) (*batchv1.Job, error) {
	// The composed odoo.conf, in a visible ConfigMap. Applied before the Job so
	// it exists by the time the pod mounts it.
	cmName := reh.Name + "-odoo-conf"
	cm := &corev1.ConfigMap{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "ConfigMap"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      cmName,
			Namespace: reh.Namespace,
			Labels:    map[string]string{"app.kubernetes.io/managed-by": "doblura"},
		},
		Data: map[string]string{"odoo.conf": odooConf(reh, st.DatabaseName)},
	}
	if err := ctrl.SetControllerReference(reh, cm, r.Scheme); err != nil {
		return nil, err
	}
	if err := r.Patch(ctx, cm, client.Apply, fieldOwner, client.ForceOwnership); err != nil {
		return nil, fmt.Errorf("applying the configuration ConfigMap: %w", err)
	}

	name := fmt.Sprintf("%s-%s", reh.Name, phaseSuffix(st.Phase))

	job := &batchv1.Job{
		TypeMeta: metav1.TypeMeta{APIVersion: "batch/v1", Kind: "Job"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: reh.Namespace,
			Labels: map[string]string{
				"app.kubernetes.io/managed-by": "doblura",
				"doblura.dev/rehearsal":         reh.Name,
				"doblura.dev/phase":             string(st.Phase),
			},
		},
		Spec: batchv1.JobSpec{
			// No retries: re-running a migration against a half-migrated
			// database fixes nothing, it makes things worse.
			BackoffLimit: ptr(int32(0)),
			Template:     r.podTemplate(reh, st),
		},
	}

	// SetControllerReference: gives us garbage collection and makes Owns() wake
	// us when the Job changes.
	if err := ctrl.SetControllerReference(reh, job, r.Scheme); err != nil {
		return nil, err
	}

	// Server-Side Apply: one call, idempotent, no 409s, and it does not stomp on
	// fields another actor manages.
	if err := r.Patch(ctx, job, client.Apply, fieldOwner, client.ForceOwnership); err != nil {
		return nil, fmt.Errorf("applying the Job for phase %s: %w", st.Phase, err)
	}

	var live batchv1.Job
	if err := r.Get(ctx, client.ObjectKeyFromObject(job), &live); err != nil {
		return nil, err
	}
	return &live, nil
}

// podTemplate builds the pod for each phase.
func (r *OdooRehearsalReconciler) podTemplate(
	reh *doblurav1alpha1.OdooRehearsal,
	st *doblurav1alpha1.OdooRehearsalStatus,
) corev1.PodTemplateSpec {
	res := sizeToResources(reh.Spec.Size)

	env := []corev1.EnvVar{
		{Name: "PGHOST", Value: reh.Spec.Database.Host},
		{Name: "PGPORT", Value: fmt.Sprint(orDefaultInt32(reh.Spec.Database.Port, 5432))},
		{Name: "PGUSER", Value: reh.Spec.Database.User},
		{Name: "PGDATABASE", Value: st.DatabaseName},
		{Name: "PGPASSWORD", ValueFrom: &corev1.EnvVarSource{
			SecretKeyRef: &corev1.SecretKeySelector{
				LocalObjectReference: corev1.LocalObjectReference{Name: reh.Spec.Database.PasswordSecret},
				Key:                  "password",
			},
		}},
	}

	volumes := []corev1.Volume{
		{
			// Required with readOnlyRootFilesystem: with no writable /tmp almost
			// nothing starts.
			Name:         "tmp",
			VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{SizeLimit: qty("1Gi")}},
		},
		{
			// Odoo's data_dir. An emptyDir because a rehearsal's filestore is
			// scratch, and because the default under $HOME is unwritable here.
			Name:         "data",
			VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}},
		},
		{
			// Where a mutating restore stages its writable copy of the dump.
			// Sized by the node, not by a limit: a production dump can be tens
			// of gigabytes and a low limit here fails as an eviction that
			// explains nothing.
			Name:         "stage",
			VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}},
		},
		{
			Name: "odoo-conf",
			VolumeSource: corev1.VolumeSource{
				ConfigMap: &corev1.ConfigMapVolumeSource{
					LocalObjectReference: corev1.LocalObjectReference{Name: reh.Name + "-odoo-conf"},
				},
			},
		},
	}
	mounts := []corev1.VolumeMount{
		{Name: "tmp", MountPath: "/tmp"},
		{Name: "data", MountPath: doblurav1alpha1.DataDirPath},
		{Name: "stage", MountPath: doblurav1alpha1.StagePath},
		// /etc/doblura and NOT /etc/odoo: mounting a ConfigMap at /etc/odoo
		// replaces the whole directory, shadowing the odoo.conf the user's image
		// ships. The official Odoo image keeps its addons_path there, so
		// mounting over it silently breaks every image that relies on its own
		// config. We bring our own directory and pass -c explicitly.
		{Name: "odoo-conf", MountPath: "/etc/doblura", ReadOnly: true},
	}

	// Addons: emptyDir for cloned repos, a ReadOnly PVC when present, and the
	// init containers that clone. Never a copy into a persistent volume.
	addonVols, addonMounts, inits := addonsPlumbing(&reh.Spec.Addons)
	volumes = append(volumes, addonVols...)
	mounts = append(mounts, addonMounts...)

	// Snapshot: the dump ends up at /snapshot wherever it came from. Only the
	// restore phase needs the fetcher; migrating and asserting already work
	// against the restored database.
	if st.Phase == doblurav1alpha1.PhaseRestoring {
		snapVols, snapMounts, snapInits := snapshotPlumbing(&reh.Spec.Snapshot)
		volumes = append(volumes, snapVols...)
		volumes = append(volumes, customExtraVolumes(&reh.Spec.Snapshot.From)...)
		mounts = append(mounts, snapMounts...)
		inits = append(inits, snapInits...)
	}

	return corev1.PodTemplateSpec{
		ObjectMeta: metav1.ObjectMeta{
			Labels: map[string]string{"doblura.dev/rehearsal": reh.Name},
		},
		Spec: corev1.PodSpec{
			RestartPolicy:  corev1.RestartPolicyNever,
			InitContainers: inits,
			SecurityContext: &corev1.PodSecurityContext{
				RunAsNonRoot: ptr(true),
				// Required: runAsNonRoot only verifies, it does not change the user.
				RunAsUser:      ptr(int64(65532)),
				SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
			},
			Containers: []corev1.Container{{
				Name:    "rehearsal",
				Image:   reh.Spec.Image,
				Command: []string{"/bin/sh", "-euc"},
				Args:    []string{phaseScript(reh, st)},
				Env:     env,
				SecurityContext: &corev1.SecurityContext{
					AllowPrivilegeEscalation: ptr(false),
					ReadOnlyRootFilesystem:   ptr(true),
					Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
				},
				Resources:    res,
				VolumeMounts: mounts,
			}},
			Volumes: volumes,
		},
	}
}

// SetupWithManager registers the controller.
func (r *OdooRehearsalReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		// GenerationChangedPredicate: the third defence against the hot loop.
		// Without it, every status write would wake us again.
		For(&doblurav1alpha1.OdooRehearsal{}, builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		Owns(&batchv1.Job{}).
		Named("odoorehearsal").
		Complete(r)
}

// ─────────────────────── helpers ───────────────────────

func (r *OdooRehearsalReconciler) hardTimeout(reh *doblurav1alpha1.OdooRehearsal) time.Duration {
	if reh.Spec.Budget != nil && reh.Spec.Budget.HardTimeout != nil {
		return reh.Spec.Budget.HardTimeout.Duration
	}
	return 6 * time.Hour
}

func jobDuration(job *batchv1.Job) time.Duration {
	if job.Status.StartTime == nil {
		return 0
	}
	end := time.Now()
	if job.Status.CompletionTime != nil {
		end = job.Status.CompletionTime.Time
	}
	return end.Sub(job.Status.StartTime.Time)
}

func phaseSuffix(p doblurav1alpha1.RehearsalPhase) string {
	switch p {
	case doblurav1alpha1.PhaseRestoring:
		return "restore"
	case doblurav1alpha1.PhaseMigrating:
		return "migrate"
	case doblurav1alpha1.PhaseAsserting:
		return "assert"
	}
	return "unknown"
}

func ptr[T any](v T) *T { return &v }

func qty(s string) *resource.Quantity {
	q := resource.MustParse(s)
	return &q
}

func orDefaultInt32(v, def int32) int32 {
	if v == 0 {
		return def
	}
	return v
}
