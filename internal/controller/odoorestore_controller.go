// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Antoni Romera

package controller

import (
	"context"
	"fmt"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	doblurav1alpha1 "github.com/doblura/doblura/api/v1alpha1"
)

// OdooRestoreReconciler puts one backup back into one environment.
//
// It runs at most once. A restore that reran because its Job was garbage
// collected would replace a database somebody had since been working in, so the
// condition is written before anything else and never removed — the object is a
// record of one event, not a loop that converges.
//
// It also SCALES THE ENVIRONMENT DOWN first. Restoring over a database Odoo is
// connected to fails at best and corrupts at worst: click-odoo-restoredb drops
// the database, and Postgres refuses to drop one with sessions on it. The
// environment comes back up afterwards, and the time between is the outage the
// person doing this already knows they are causing.
type OdooRestoreReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=doblura.dev,resources=odoorestores,verbs=get;list;watch
// +kubebuilder:rbac:groups=doblura.dev,resources=odoorestores/status,verbs=get;update;patch

const restoreDoneCondition = "Restored"

func (r *OdooRestoreReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var rs doblurav1alpha1.OdooRestore
	if err := r.Get(ctx, req.NamespacedName, &rs); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	st := rs.Status.DeepCopy()
	st.ObservedGeneration = rs.Generation

	// Terminal, and it stays terminal. Nothing below this line runs twice.
	if c := meta.FindStatusCondition(st.Conditions, restoreDoneCondition); c != nil &&
		(c.Status == metav1.ConditionTrue || c.Reason == "Failed") {
		return ctrl.Result{}, nil
	}

	backup, env, err := r.restoreTargets(ctx, &rs)
	if err != nil {
		return r.failRestore(ctx, &rs, st, "TargetMissing", err)
	}

	// The copy has to exist. Checked against what the backup's status says is on
	// the volume rather than trusting the name: a typo would otherwise produce a
	// Job that scales the environment down, finds nothing, and leaves it down.
	if !backupHasCopy(backup, rs.Spec.Copy) {
		return r.failRestore(ctx, &rs, st, "NoSuchCopy", fmt.Errorf(
			"backup %q has no copy called %q. It keeps %d: %s",
			rs.Spec.Backup, rs.Spec.Copy, len(backup.Status.Copies),
			copyNames(backup)))
	}

	if st.StartedAt == nil {
		now := metav1.Now()
		st.StartedAt = &now
		st.Phase = doblurav1alpha1.RestoreRestoring
	}
	// Copied from the annotation the admission webhook stamped, so the field
	// people read is on the object where they look for it. Never from anything
	// the client sent.
	st.RequestedBy = rs.Annotations["doblura.dev/requested-by"]

	// Down before, up after. Written as the environment's own field so the
	// operator's normal reconcile does it, rather than this controller reaching
	// into a Deployment that another controller owns.
	if err := r.setHibernated(ctx, env, true); err != nil {
		return r.failRestore(ctx, &rs, st, "CannotStop", err)
	}

	job, err := r.ensureRestoreJob(ctx, &rs, backup, env)
	if err != nil {
		return r.failRestore(ctx, &rs, st, "CannotStart", err)
	}

	switch {
	case job.Status.Succeeded > 0:
		if err := r.setHibernated(ctx, env, false); err != nil {
			return r.failRestore(ctx, &rs, st, "CannotStart", err)
		}
		now := metav1.Now()
		st.FinishedAt, st.Phase = &now, doblurav1alpha1.RestoreSucceeded
		st.Message = fmt.Sprintf("%s restored into %s", rs.Spec.Copy, rs.Spec.Into)
		meta.SetStatusCondition(&st.Conditions, metav1.Condition{
			Type: restoreDoneCondition, Status: metav1.ConditionTrue,
			Reason: "Restored", Message: st.Message, ObservedGeneration: rs.Generation,
		})
		return r.commitRestore(ctx, &rs, st, ctrl.Result{})

	case job.Status.Failed > 0:
		// The environment is brought back up even though the restore failed. It
		// may be running a half-restored database and that is bad — but leaving
		// it switched off with no explanation is worse, and the status says
		// exactly what happened.
		_ = r.setHibernated(ctx, env, false)
		return r.failRestore(ctx, &rs, st, "Failed", fmt.Errorf(
			"the restore failed; the environment has been started again, but its "+
				"database may be half-restored. Read the logs of Job %s", job.Name))
	}

	return r.commitRestore(ctx, &rs, st, ctrl.Result{RequeueAfter: 15 * time.Second})
}

func (r *OdooRestoreReconciler) restoreTargets(
	ctx context.Context,
	rs *doblurav1alpha1.OdooRestore,
) (*doblurav1alpha1.OdooBackup, *doblurav1alpha1.OdooEnvironment, error) {
	var b doblurav1alpha1.OdooBackup
	if err := r.Get(ctx, client.ObjectKey{
		Namespace: rs.Namespace, Name: rs.Spec.Backup,
	}, &b); err != nil {
		return nil, nil, fmt.Errorf("no OdooBackup called %q in this namespace", rs.Spec.Backup)
	}
	var e doblurav1alpha1.OdooEnvironment
	if err := r.Get(ctx, client.ObjectKey{
		Namespace: rs.Namespace, Name: rs.Spec.Into,
	}, &e); err != nil {
		return nil, nil, fmt.Errorf("no OdooEnvironment called %q in this namespace", rs.Spec.Into)
	}
	return &b, &e, nil
}

func backupHasCopy(b *doblurav1alpha1.OdooBackup, name string) bool {
	for _, c := range b.Status.Copies {
		if c.Name == name {
			return true
		}
	}
	return false
}

func copyNames(b *doblurav1alpha1.OdooBackup) string {
	if len(b.Status.Copies) == 0 {
		return "none — it has not taken one yet, or the last run failed"
	}
	out := ""
	for i, c := range b.Status.Copies {
		if i > 0 {
			out += ", "
		}
		out += c.Name
	}
	return out
}

// setHibernated switches the environment off and on through its own API.
func (r *OdooRestoreReconciler) setHibernated(
	ctx context.Context,
	env *doblurav1alpha1.OdooEnvironment,
	down bool,
) error {
	var live doblurav1alpha1.OdooEnvironment
	if err := r.Get(ctx, client.ObjectKeyFromObject(env), &live); err != nil {
		return err
	}
	want := doblurav1alpha1.LifecyclePersistent
	if down {
		want = doblurav1alpha1.LifecycleHibernating
	}
	// Only touched when it differs, and never over an Ephemeral lifecycle: an
	// ephemeral environment's ttl is what removes it, and rewriting its type
	// would leave it alive for ever.
	if live.Spec.Lifecycle.Type == doblurav1alpha1.LifecycleEphemeral {
		return nil
	}
	if live.Spec.Lifecycle.Type == want {
		return nil
	}
	live.Spec.Lifecycle.Type = want
	return r.Update(ctx, &live)
}

func (r *OdooRestoreReconciler) ensureRestoreJob(
	ctx context.Context,
	rs *doblurav1alpha1.OdooRestore,
	b *doblurav1alpha1.OdooBackup,
	env *doblurav1alpha1.OdooEnvironment,
) (*batchv1.Job, error) {
	name := "restore-" + rs.Name
	var live batchv1.Job
	err := r.Get(ctx, client.ObjectKey{Namespace: rs.Namespace, Name: name}, &live)
	if err == nil {
		return &live, nil
	}
	if !errors.IsNotFound(err) {
		return nil, err
	}

	timeout := int64(4 * time.Hour / time.Second)
	if t := rs.Spec.Timeout; t != nil && t.Duration > 0 {
		timeout = int64(t.Duration / time.Second)
	}

	pod := envJobPod(env, envPhaseStep{"restore", "", "", func(*doblurav1alpha1.OdooEnvironment) string {
		return backupRestoreScript(rs, b, env)
	}})
	vols, mounts := backupDestination(&b.Spec.Destination)
	pod.Spec.Volumes = append(pod.Spec.Volumes, vols...)
	for i := range pod.Spec.Containers {
		pod.Spec.Containers[i].VolumeMounts = append(pod.Spec.Containers[i].VolumeMounts, mounts...)
	}

	job := &batchv1.Job{
		TypeMeta: metav1.TypeMeta{APIVersion: "batch/v1", Kind: "Job"},
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: rs.Namespace,
			Labels: map[string]string{
				"app.kubernetes.io/managed-by": "doblura",
				"doblura.dev/restore":          rs.Name,
			},
		},
		Spec: batchv1.JobSpec{
			BackoffLimit:          ptr(int32(0)),
			ActiveDeadlineSeconds: &timeout,
			Template:              pod,
		},
	}
	if err := ctrl.SetControllerReference(rs, job, r.Scheme); err != nil {
		return nil, err
	}
	if err := r.Create(ctx, job); err != nil && !errors.IsAlreadyExists(err) {
		return nil, err
	}
	return job, nil
}

// restoreScript puts the copy back.
// backupRestoreScript is named for what it restores FROM: a plain rehearsal
// restoreScript already existed for snapshots, and two functions with one name
// is how the wrong one gets called.
func backupRestoreScript(
	rs *doblurav1alpha1.OdooRestore,
	b *doblurav1alpha1.OdooBackup,
	env *doblurav1alpha1.OdooEnvironment,
) string {
	db := envDBName(env)
	file := fmt.Sprintf("%s/%s/%s.zip", backupMountPath, b.Name, rs.Spec.Copy)

	neutralize := ""
	if rs.Spec.NeutralizesRestore() {
		// click-odoo-restoredb's own flag, rather than a separate pass: it runs
		// inside the restore transaction, so there is no window in which the
		// database is live with production's crons enabled.
		neutralize = " --neutralize"
	}

	// --copy or --move, and this is not a detail.
	//
	// Odoo records a uuid per database and uses it to identify itself — to its
	// own mail gateway, to the enterprise service, to anything that pairs an
	// installation with a subscription. Restoring into a DIFFERENT environment
	// without saying so leaves two databases claiming to be the same
	// installation, which is a class of problem that surfaces days later as mail
	// disappearing or a subscription being reported as duplicated.
	//
	// Doblura knows which case it is, because the backup records the environment
	// it came from. Restoring a database back where it came from is a move;
	// anything else is a copy, and gets a new identity.
	movement := " --copy"
	if b.Spec.Environment == rs.Spec.Into {
		movement = " --move"
	}

	// --force, because the destination exists: click-odoo-restoredb refuses
	// otherwise, and dropping it by hand first would leave a window where the
	// environment has no database at all and the restore has not started.
	force := " --force"

	return fmt.Sprintf(`
if ! command -v click-odoo-restoredb >/dev/null 2>&1; then
  echo "!! this image does not ship click-odoo-contrib, and restoring needs it." >&2
  exit 1
fi
if [ ! -f "%[1]s" ]; then
  echo "!! %[1]s is not on the volume. The backup's status lists what is." >&2
  exit 1
fi

echo ">> restoring %[2]s from %[1]s"
echo ">> this REPLACES the database and filestore of %[3]s"
echo ">> restoring as a$(echo "%[6]s" | tr -d ' -')"
click-odoo-restoredb -c %[4]s%[5]s%[6]s%[7]s "%[2]s" "%[1]s"
echo ">> restored"
`, file, db, env.Name, envConf, neutralize, movement, force)
}

func (r *OdooRestoreReconciler) failRestore(
	ctx context.Context,
	rs *doblurav1alpha1.OdooRestore,
	st *doblurav1alpha1.OdooRestoreStatus,
	reason string,
	cause error,
) (ctrl.Result, error) {
	now := metav1.Now()
	st.Phase, st.FinishedAt, st.Message = doblurav1alpha1.RestoreFailed, &now, cause.Error()
	meta.SetStatusCondition(&st.Conditions, metav1.Condition{
		Type: restoreDoneCondition, Status: metav1.ConditionFalse,
		Reason: reason, Message: cause.Error(), ObservedGeneration: rs.Generation,
	})
	// No requeue. A restore that failed is not retried on its own: whether to
	// try again is a decision, and making it automatically would replace the
	// database a second time while somebody was looking at the first failure.
	return r.commitRestore(ctx, rs, st, ctrl.Result{})
}

func (r *OdooRestoreReconciler) commitRestore(
	ctx context.Context,
	rs *doblurav1alpha1.OdooRestore,
	st *doblurav1alpha1.OdooRestoreStatus,
	res ctrl.Result,
) (ctrl.Result, error) {
	if equality.Semantic.DeepEqual(&rs.Status, st) {
		return res, nil
	}
	rs.Status = *st
	return res, r.Status().Update(ctx, rs)
}

func (r *OdooRestoreReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&doblurav1alpha1.OdooRestore{}).
		Owns(&batchv1.Job{}).
		Named("odoorestore").
		Complete(r)
}
