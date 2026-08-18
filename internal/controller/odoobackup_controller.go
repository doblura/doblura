// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Antoni Romera

package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	doblurav1alpha1 "github.com/doblura/doblura/api/v1alpha1"
)

// OdooBackupReconciler keeps copies of an environment and removes the ones it no
// longer needs.
//
// The division of labour is the point. The MANAGER decides what to keep, in Go,
// from a list of what exists — the retention policy is tested without a cluster
// because it decides what gets deleted. The JOB does everything that touches
// data: it prunes what the manager decided last time, takes a new copy, and
// lists what is now there. The manager never mounts the volume and never holds
// the database credential, exactly as everywhere else here.
//
// One Job per run rather than three, because the three steps have to happen in
// order against the same volume and splitting them would mean a prune that ran
// after a backup it was not told about.
type OdooBackupReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=doblura.dev,resources=odoobackups,verbs=get;list;watch
// +kubebuilder:rbac:groups=doblura.dev,resources=odoobackups/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=batch,resources=cronjobs,verbs=get;list;watch;create;update;patch;delete

func (r *OdooBackupReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var b doblurav1alpha1.OdooBackup
	if err := r.Get(ctx, req.NamespacedName, &b); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	st := b.Status.DeepCopy()
	st.ObservedGeneration = b.Generation

	var env doblurav1alpha1.OdooEnvironment
	if err := r.Get(ctx, client.ObjectKey{
		Namespace: b.Namespace, Name: b.Spec.Environment,
	}, &env); err != nil {
		if errors.IsNotFound(err) {
			// Named an environment that is not there. Said plainly and NOT
			// treated as an error to retry for ever: the fix is a person editing
			// the name, and a controller hammering the API server about it helps
			// nobody.
			return r.failBackup(ctx, &b, st, "NoSuchEnvironment", fmt.Errorf(
				"there is no OdooEnvironment called %q in this namespace", b.Spec.Environment))
		}
		return ctrl.Result{}, err
	}

	// Read back whatever the last run reported, and decide from it.
	if listing := r.lastListing(ctx, &b); listing != nil {
		keep, drop := doblurav1alpha1.Retain(listing, b.Spec.Retention, time.Now())
		st.Copies = keep
		st.Kept = int32(len(keep)) //nolint:gosec // bounded by the retention schema
		st.Pending = names(drop)
		for i := range st.Copies {
			st.Copies[i].Tier = doblurav1alpha1.TierOf(st.Copies[i], b.Spec.Retention, listing)
		}
	}

	if err := r.ensureCronJob(ctx, &b, &env, st); err != nil {
		return r.failBackup(ctx, &b, st, "CannotSchedule", err)
	}

	reason, message := "Scheduled", fmt.Sprintf(
		"%d kept; next run %s", st.Kept, b.Spec.Schedule)
	status := metav1.ConditionTrue
	if b.Spec.Suspend {
		reason, message = "Suspended", "suspended; no new copies are being taken"
		status = metav1.ConditionFalse
	} else if st.LastSuccess == nil && st.LastRun != nil {
		// Ran and never succeeded. A schedule that has run every night for a
		// week and failed every night looks busy, and this is the only line that
		// says otherwise.
		reason, status = "NeverSucceeded", metav1.ConditionFalse
		message = "it has run but never completed; there is no backup"
	}
	meta.SetStatusCondition(&st.Conditions, metav1.Condition{
		Type: "Backing", Status: status, Reason: reason,
		Message: message, ObservedGeneration: b.Generation,
	})

	return r.commitBackup(ctx, &b, st, ctrl.Result{RequeueAfter: time.Minute})
}

// lastListing reads what the most recent run said was on the volume.
func (r *OdooBackupReconciler) lastListing(
	ctx context.Context,
	b *doblurav1alpha1.OdooBackup,
) []doblurav1alpha1.BackupCopy {
	var pods corev1.PodList
	if err := r.List(ctx, &pods, client.InNamespace(b.Namespace),
		client.MatchingLabels{"doblura.dev/backup": b.Name}); err != nil {
		return nil
	}

	// Newest FIRST, and then the newest that actually reported something.
	//
	// This used to take the newest pod outright, which is the one still running
	// while a backup is in progress — no termination message, so it gave up and
	// the status kept an older reading. The volume had two copies and the status
	// said one, with nothing pending: the retention policy was being applied to
	// a list that was one run out of date, so a copy that should have been
	// deleted simply was not.
	sort.Slice(pods.Items, func(i, j int) bool {
		a, b := pods.Items[i].Status.StartTime, pods.Items[j].Status.StartTime
		switch {
		case a == nil:
			return false
		case b == nil:
			return true
		}
		return a.After(b.Time)
	})

	for i := range pods.Items {
		for _, cs := range pods.Items[i].Status.ContainerStatuses {
			t := cs.State.Terminated
			if t == nil || t.Message == "" {
				continue
			}
			var listing []doblurav1alpha1.BackupCopy
			if err := json.Unmarshal([]byte(lastJSONLine(t.Message)), &listing); err == nil {
				return listing
			}
		}
	}
	return nil
}

func lastJSONLine(msg string) string {
	out := "[]"
	for _, l := range strings.Split(strings.TrimSpace(msg), "\n") {
		if l = strings.TrimSpace(l); strings.HasPrefix(l, "[") {
			out = l
		}
	}
	return out
}

func names(in []doblurav1alpha1.BackupCopy) []string {
	if len(in) == 0 {
		return nil
	}
	out := make([]string, 0, len(in))
	for _, c := range in {
		out = append(out, c.Name)
	}
	return out
}

func (r *OdooBackupReconciler) failBackup(
	ctx context.Context,
	b *doblurav1alpha1.OdooBackup,
	st *doblurav1alpha1.OdooBackupStatus,
	reason string,
	cause error,
) (ctrl.Result, error) {
	st.Message = cause.Error()
	meta.SetStatusCondition(&st.Conditions, metav1.Condition{
		Type: "Backing", Status: metav1.ConditionFalse, Reason: reason,
		Message: cause.Error(), ObservedGeneration: b.Generation,
	})
	return r.commitBackup(ctx, b, st, ctrl.Result{RequeueAfter: 10 * time.Minute})
}

func (r *OdooBackupReconciler) commitBackup(
	ctx context.Context,
	b *doblurav1alpha1.OdooBackup,
	st *doblurav1alpha1.OdooBackupStatus,
	res ctrl.Result,
) (ctrl.Result, error) {
	if equality.Semantic.DeepEqual(&b.Status, st) {
		return res, nil
	}
	b.Status = *st
	return res, r.Status().Update(ctx, b)
}

func (r *OdooBackupReconciler) SetupWithManager(mgr ctrl.Manager) error {
	// Watches Jobs as well as the CronJob, because the CronJob is not what
	// produces the answer — a run finishing is. Without this the controller woke
	// only on its requeue, so the volume held three copies while the status said
	// one and the retention policy was being applied to a list up to five
	// minutes out of date.
	//
	// Jobs and not Pods: the Job is what the CronJob creates, it carries the
	// label, and there are far fewer of them.
	return ctrl.NewControllerManagedBy(mgr).
		For(&doblurav1alpha1.OdooBackup{}).
		Owns(&batchv1.CronJob{}).
		Watches(&batchv1.Job{}, backupOfJob()).
		Named("odoobackup").
		Complete(r)
}

// backupOfJob maps a backup Job back to the OdooBackup it belongs to.
func backupOfJob() handler.EventHandler {
	return handler.EnqueueRequestsFromMapFunc(
		func(_ context.Context, obj client.Object) []reconcile.Request {
			name := obj.GetLabels()["doblura.dev/backup"]
			if name == "" {
				return nil
			}
			return []reconcile.Request{{
				NamespacedName: types.NamespacedName{Namespace: obj.GetNamespace(), Name: name},
			}}
		})
}

// ensureCronJob writes the schedule.
//
// A CronJob rather than the manager timing it: Kubernetes already knows how to
// run something nightly, survive its own restart and not run two at once, and
// reimplementing that in a reconcile loop is how a backup gets skipped on the
// night the operator was being upgraded.
func (r *OdooBackupReconciler) ensureCronJob(
	ctx context.Context,
	b *doblurav1alpha1.OdooBackup,
	env *doblurav1alpha1.OdooEnvironment,
	st *doblurav1alpha1.OdooBackupStatus,
) error {
	name := b.Name + "-backup"
	suspend := b.Spec.Suspend
	timeout := int64(2 * time.Hour / time.Second)
	if t := b.Spec.Timeout; t != nil && t.Duration > 0 {
		timeout = int64(t.Duration / time.Second)
	}

	pod := envJobPod(env, envPhaseStep{"backup", "", "", func(*doblurav1alpha1.OdooEnvironment) string {
		return backupScript(b, env, st.Pending)
	}})
	pod.Labels["doblura.dev/backup"] = b.Name

	// The destination is mounted here and nowhere else.
	vols, mounts := backupDestination(&b.Spec.Destination)
	pod.Spec.Volumes = append(pod.Spec.Volumes, vols...)
	for i := range pod.Spec.Containers {
		pod.Spec.Containers[i].VolumeMounts = append(pod.Spec.Containers[i].VolumeMounts, mounts...)
		pod.Spec.Containers[i].TerminationMessagePath = backupListingPath
		pod.Spec.Containers[i].TerminationMessagePolicy = corev1.TerminationMessageFallbackToLogsOnError
	}

	cj := &batchv1.CronJob{
		TypeMeta: metav1.TypeMeta{APIVersion: "batch/v1", Kind: "CronJob"},
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: b.Namespace,
			Labels: map[string]string{
				"app.kubernetes.io/managed-by": "doblura",
				"doblura.dev/backup":           b.Name,
			},
		},
		Spec: batchv1.CronJobSpec{
			Schedule: b.Spec.Schedule,
			Suspend:  &suspend,
			// Forbid, never Allow: two backups of one database at once means two
			// pg_dumps competing for the same disk and, with retention in the
			// mix, one deleting what the other is writing.
			ConcurrencyPolicy:          batchv1.ForbidConcurrent,
			SuccessfulJobsHistoryLimit: ptr(int32(3)),
			FailedJobsHistoryLimit:     ptr(int32(3)),
			JobTemplate: batchv1.JobTemplateSpec{
				// The label goes on the JOB as well as its pod: the watch above
				// maps Jobs back to their backup, and a Job without it would
				// finish without waking anything.
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{"doblura.dev/backup": b.Name},
				},
				Spec: batchv1.JobSpec{
					BackoffLimit:          ptr(int32(1)),
					ActiveDeadlineSeconds: &timeout,
					Template:              pod,
				},
			},
		},
	}
	if err := ctrl.SetControllerReference(b, cj, r.Scheme); err != nil {
		return err
	}
	return r.Patch(ctx, cj, client.Apply, fieldOwner, client.ForceOwnership)
}

const (
	backupMountPath   = "/backups"
	backupListingPath = "/tmp/listing"
)

func backupDestination(d *doblurav1alpha1.SnapshotDestination) ([]corev1.Volume, []corev1.VolumeMount) {
	if d.Volume == nil {
		return nil, nil
	}
	return []corev1.Volume{{
			Name: "backups",
			VolumeSource: corev1.VolumeSource{
				PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
					ClaimName: d.Volume.ClaimName,
				},
			},
		}}, []corev1.VolumeMount{{
			Name: "backups", MountPath: backupMountPath,
		}}
}

// backupScript prunes, backs up, and lists — in that order.
//
// Prune FIRST, so the disk has room for the copy about to be written. A backup
// that fails because the volume is full, on a volume holding copies the policy
// had already decided to delete, is a failure that did not need to happen.
func backupScript(
	b *doblurav1alpha1.OdooBackup,
	env *doblurav1alpha1.OdooEnvironment,
	pending []string,
) string {
	db := envDBName(env)
	dir := backupMountPath + "/" + b.Name

	var prune strings.Builder
	for _, name := range pending {
		// Names come from a listing this operator produced and are matched
		// against what is on the volume; still, the deletion is scoped to this
		// backup's own directory and refuses anything with a slash in it.
		if strings.ContainsAny(name, "/ ") {
			continue
		}
		// The .zip matters. The listing strips the extension so the name reads
		// like a timestamp, and this deleted the bare name — a path that does not
		// exist, which `rm -rf` removes successfully and silently. The prune
		// reported three removals, deleted nothing, and the volume grew for ever
		// while the status said the policy was being applied.
		//
		// It now checks the file was there, so a prune that removes nothing says
		// so rather than claiming success.
		fmt.Fprintf(&prune, `if [ -e "%[1]s/%[2]s.zip" ]; then
  rm -f -- "%[1]s/%[2]s.zip" && echo ">> removed %[2]s"
else
  echo ">> %[2]s was already gone" >&2
fi
`, dir, name)
	}

	return fmt.Sprintf(`
if ! command -v click-odoo-backupdb >/dev/null 2>&1; then
  echo "!! this image does not ship click-odoo-contrib, and backing up needs it." >&2
  exit 1
fi
mkdir -p "%[1]s"

# What the retention policy decided last time. Done before the new copy is
# written, so the volume has room for it.
%[2]s
STAMP=$(date -u +%%Y-%%m-%%dT%%H-%%M-%%SZ)
echo ">> backing up %[3]s as $STAMP"
click-odoo-backupdb -c %[4]s "%[3]s" "%[1]s/$STAMP.zip"
echo ">> backup finished"

# Everything that is now there, newest last, as JSON for the operator to apply
# the policy to. Sizes in bytes; the timestamp comes from the NAME rather than
# from the filesystem, because a copy restored onto a new volume keeps its name
# and loses its mtime.
{
  printf '['
  first=1
  for f in "%[1]s"/*.zip; do
    [ -e "$f" ] || continue
    n=$(basename "$f" .zip)
    sz=$(wc -c < "$f" | tr -d ' ')
    iso=$(echo "$n" | sed -E 's/^([0-9]{4}-[0-9]{2}-[0-9]{2})T([0-9]{2})-([0-9]{2})-([0-9]{2})Z$/\1T\2:\3:\4Z/')
    [ $first -eq 1 ] || printf ','
    first=0
    printf '{"name":"%%s","takenAt":"%%s","sizeBytes":%%s}' "$n" "$iso" "$sz"
  done
  printf ']\n'
} | tee %[5]s
`, dir, prune.String(), db, envConf, backupListingPath)
}
