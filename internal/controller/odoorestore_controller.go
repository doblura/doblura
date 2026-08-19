// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Antoni Romera

package controller

import (
	"context"
	"fmt"
	"strings"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
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
		// Unless doblura wrote it itself, as the copy taken before an earlier
		// restore. Those are on the volume the moment the restore finishes, and
		// they are NOT in status.copies until the backup's next scheduled run
		// re-lists the volume — which on a nightly schedule is up to a day away.
		//
		// That gap made the one thing the safety copy exists for impossible:
		// undoing a bad restore was refused, with a message saying the copy did
		// not exist, while the file sat on the volume. Found by trying it.
		//
		// This is not a hole in the check. The check exists so a typo does not
		// take an environment down for nothing, and a name doblura recorded
		// itself is not a typo. The Job still refuses if the file is not there.
		if from := r.recordedSafetyCopy(ctx, &rs); from == "" {
			return r.failRestore(ctx, &rs, st, "NoSuchCopy", fmt.Errorf(
				"backup %q has no copy called %q. It keeps %d: %s",
				rs.Spec.Backup, rs.Spec.Copy, len(backup.Status.Copies),
				copyNames(backup)))
		}
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

	// Where the copy of the CURRENT contents goes, if one is being taken. The
	// webhook already refused the cases where there is nowhere to put it, so an
	// empty answer here means no copy was asked for.
	safety, err := r.safetyDestination(ctx, &rs, env, backup)
	if err != nil {
		return r.failRestore(ctx, &rs, st, "NoSafetyDestination", err)
	}

	job, err := r.ensureRestoreJob(ctx, &rs, backup, env, safety)
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
		// The way back, recorded on the object that did the replacing. Read from
		// what the Job printed rather than computed here: the Job is what chose
		// the timestamp, and a name computed twice is a name that can differ.
		if safety != nil {
			if taken := r.safetyCopyTaken(ctx, &rs); taken != "" {
				st.SafetyCopy, st.SafetyCopyIn = taken, safety.Backup
				st.Message += fmt.Sprintf(
					"; what was in it is copy %s of backup %s", taken, safety.Backup)
			}
		}
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
	safety *safetyDestination,
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
		return backupRestoreScript(rs, b, env, safety)
	}})
	vols, mounts := backupDestination(&b.Spec.Destination)

	// A second mount when the safety copy goes to a different volume, which is
	// the normal case: the copy of what is in the target belongs on the target's
	// own backup volume, and the copy being restored came from wherever it came
	// from. Same claim, one mount — Kubernetes refuses two volumes with one name,
	// and two mounts of one claim at two paths is pointless.
	if safety != nil && safety.Separate {
		vols = append(vols, corev1.Volume{
			Name: safetyVolumeName,
			VolumeSource: corev1.VolumeSource{
				PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
					ClaimName: safety.ClaimName,
				},
			},
		})
		mounts = append(mounts, corev1.VolumeMount{
			Name: safetyVolumeName, MountPath: safetyMountPath,
		})
	}

	pod.Spec.Volumes = append(pod.Spec.Volumes, vols...)
	for i := range pod.Spec.Containers {
		pod.Spec.Containers[i].VolumeMounts = append(pod.Spec.Containers[i].VolumeMounts, mounts...)
		// The name of the copy comes back through the termination message, like
		// every other read-back in this operator. The first version searched the
		// pod's LOGS for the marker and found nothing twice over: the label it
		// filtered on was set on the Job and Kubernetes does not copy Job labels
		// onto pods, and even with the right label a successful container's
		// message is the file's contents and not its output. So the script writes
		// the file, and the restore's status stopped being silently empty.
		pod.Spec.Containers[i].TerminationMessagePath = safetyNamePath
		pod.Spec.Containers[i].TerminationMessagePolicy =
			corev1.TerminationMessageFallbackToLogsOnError
	}

	// And the pods carry the label the read-back filters on, which is a different
	// field from the Job's own labels.
	if pod.Labels == nil {
		pod.Labels = map[string]string{}
	}
	pod.Labels["doblura.dev/restore"] = rs.Name

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
	safety *safetyDestination,
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

	return safetyCopyScript(env, safety) + fmt.Sprintf(`
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
echo ">> restoring as a $(echo "%[6]s" | tr -d ' -')"
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

// ─────────────── the copy taken before the replacing ───────────────
//
// A restore is the one action in doblura that destroys data on purpose, and the
// acknowledgement naming the target only catches one of the two ways it goes
// wrong. The other one — the right environment, the wrong copy — is caught by
// nothing a person types, and is only survivable if what was there is still
// somewhere.
//
// So the safety copy is taken by the SAME Job, before the restore, with `set -e`
// meaning a failed copy is a Job that never reaches the restore. That ordering is
// the whole design: refusing to replace a database is recoverable, replacing it
// with no way back is not.

const (
	safetyVolumeName = "safety"
	safetyMountPath  = "/safety"
	// safetyNamePath is where the Job leaves the name of the copy it took, for
	// Kubernetes to lift into the container's termination message.
	safetyNamePath = "/tmp/safety-copy"
)

// safetyDestination is where the copy of the current contents goes.
type safetyDestination struct {
	// Backup is the OdooBackup that owns the volume, and whose retention policy
	// will eventually prune the copy like any other.
	Backup string
	// ClaimName is its volume.
	ClaimName string
	// Separate says the copy needs a mount of its own, because it is going to a
	// different volume from the one the restore reads. Decided once, here, and
	// read by both the pod builder and the script.
	Separate bool
	// Dir is the path the Job writes to, already resolved to whichever mount the
	// pod ended up with.
	Dir string
}

// sourceClaim is the claim the copy being restored comes from.
func sourceClaim(b *doblurav1alpha1.OdooBackup) string {
	if b.Spec.Destination.Volume == nil {
		return ""
	}
	return b.Spec.Destination.Volume.ClaimName
}

// safetyDestination finds where a copy of the target's current contents can go.
//
// nil, nil means none is being taken, which the webhook has already decided —
// including refusing the cases where one was required and there was nowhere to
// put it. This re-derives the destination rather than reading it off an
// annotation the webhook wrote, because an annotation is something a client can
// set and this decides which volume gets written to.
func (r *OdooRestoreReconciler) safetyDestination(
	ctx context.Context,
	rs *doblurav1alpha1.OdooRestore,
	env *doblurav1alpha1.OdooEnvironment,
	source *doblurav1alpha1.OdooBackup,
) (*safetyDestination, error) {
	if !rs.Spec.TakesSafetyCopy() {
		return nil, nil
	}

	var list doblurav1alpha1.OdooBackupList
	if err := r.List(ctx, &list, client.InNamespace(rs.Namespace)); err != nil {
		return nil, err
	}
	var chosen *doblurav1alpha1.OdooBackup
	for i := range list.Items {
		b := &list.Items[i]
		if b.Spec.Environment != env.Name || b.Spec.Destination.Volume == nil {
			continue
		}
		if chosen == nil || b.Name < chosen.Name {
			chosen = b
		}
	}
	if chosen == nil {
		// The webhook refuses this on the way in, so reaching it means the
		// backup was deleted between the request and now. Refused rather than
		// quietly restoring without a copy: the person who asked was told there
		// would be one.
		return nil, fmt.Errorf(
			"a copy of %s was to be taken first, but no OdooBackup with a volume "+
				"is copying it any more — it was deleted after this restore was "+
				"asked for. Nothing has been changed", env.Name)
	}

	// One decision, made once. Whether the copy needs its own mount and where the
	// Job writes it are the same question, and the first version of this answered
	// it in two places — which writes the copy into the SOURCE volume the moment
	// the two answers disagree, where the target's retention never sees it and it
	// sits for ever.
	claim := chosen.Spec.Destination.Volume.ClaimName
	separate := claim != sourceClaim(source)
	root := backupMountPath
	if separate {
		root = safetyMountPath
	}

	return &safetyDestination{
		Backup:    chosen.Name,
		ClaimName: claim,
		Separate:  separate,
		Dir:       root + "/" + chosen.Name,
	}, nil
}

// safetyCopyTaken reads back the name the Job chose.
//
// From the termination message, like every other read-back in this operator: the
// Job picked the timestamp, and a name recomputed here would drift by however long
// the Job waited to start.
func (r *OdooRestoreReconciler) safetyCopyTaken(
	ctx context.Context,
	rs *doblurav1alpha1.OdooRestore,
) string {
	var pods corev1.PodList
	if err := r.List(ctx, &pods, client.InNamespace(rs.Namespace),
		client.MatchingLabels{"doblura.dev/restore": rs.Name}); err != nil {
		return ""
	}
	for i := range pods.Items {
		for _, cs := range pods.Items[i].Status.ContainerStatuses {
			if cs.State.Terminated == nil {
				continue
			}
			for _, line := range strings.Split(cs.State.Terminated.Message, "\n") {
				line = strings.TrimSpace(line)
				if rest, ok := strings.CutPrefix(line, safetyMarker); ok {
					return strings.TrimSpace(rest)
				}
				// On a failure the message is the tail of the LOG rather than the
				// file, so the marker arrives among the ordinary output. Both
				// shapes are read, because the failure case is the one where
				// somebody most needs to know whether the copy exists.
				if i := strings.Index(line, safetyMarker); i >= 0 {
					return strings.TrimSpace(line[i+len(safetyMarker):])
				}
			}
		}
	}
	return ""
}

// safetyMarker is how the Job names the copy it took. One line, one prefix, so
// the operator is not parsing prose.
const safetyMarker = "SAFETY-COPY="

// safetyCopyScript is the part of the restore that runs first.
func safetyCopyScript(env *doblurav1alpha1.OdooEnvironment, dest *safetyDestination) string {
	if dest == nil {
		return `
echo ">> no copy is being taken of this environment before it is replaced"
`
	}
	db := envDBName(env)
	return fmt.Sprintf(`
if ! command -v click-odoo-backupdb >/dev/null 2>&1; then
  echo "!! this image does not ship click-odoo-contrib, and the copy taken before" >&2
  echo "!! replacing the database needs it. Nothing has been changed." >&2
  exit 1
fi

mkdir -p "%[1]s"
SAFETY=$(date -u +%%Y-%%m-%%dT%%H-%%M-%%SZ)
echo ">> copying what is in %[2]s NOW, before replacing it"
echo ">> into %[1]s/$SAFETY.zip"

# Not "|| true". A failed copy has to stop the restore, because the copy is the
# only thing that makes the restore undoable — and the whole point is that
# refusing to replace a database is recoverable and replacing it blind is not.
click-odoo-backupdb -c %[3]s "%[2]s" "%[1]s/$SAFETY.zip"

# The name, written where the operator reads it back and echoed for whoever is
# watching the log. It is what somebody restores to undo this.
printf '%[5]s%%s\n' "$SAFETY" > %[6]s
echo "%[4]s$SAFETY"
echo ">> copy taken; the restore follows"
`, dest.Dir, db, envConf, safetyMarker, safetyMarker, safetyNamePath)
}

// recordedSafetyCopy reports whether this copy is one doblura took itself, before
// an earlier restore replaced the same environment.
//
// Returns the name of the restore that took it, for the message, or "" if none
// did. Searched across the namespace rather than tracked in a field on the backup,
// because the backup's status is rewritten wholesale from whatever the last
// listing said — anything this controller wrote there would be erased by the next
// reconcile, which is a worse kind of wrong than a small List.
func (r *OdooRestoreReconciler) recordedSafetyCopy(
	ctx context.Context,
	rs *doblurav1alpha1.OdooRestore,
) string {
	var list doblurav1alpha1.OdooRestoreList
	if err := r.List(ctx, &list, client.InNamespace(rs.Namespace)); err != nil {
		return ""
	}
	for i := range list.Items {
		other := &list.Items[i]
		if other.Name == rs.Name {
			continue
		}
		if other.Status.SafetyCopy == rs.Spec.Copy &&
			other.Status.SafetyCopyIn == rs.Spec.Backup {
			return other.Name
		}
	}
	return ""
}
