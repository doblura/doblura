package controller

import (
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"

	doblurav1alpha1 "github.com/doblura/doblura/api/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// The whole point of the split, expressed as the one line that must be zero.
func TestWebTierDoesNotRunCronsWhenACronTierExists(t *testing.T) {
	env := &doblurav1alpha1.OdooEnvironment{
		ObjectMeta: metav1.ObjectMeta{Name: "e", Namespace: "d"},
		Spec: doblurav1alpha1.OdooEnvironmentSpec{
			Image:    "img",
			Database: doblurav1alpha1.DatabaseSpec{Host: "h", User: "u", PasswordSecret: "s"},
		},
	}
	// No split: the historical behaviour, one deployment doing both.
	if got := envOdooConf(env); !strings.Contains(got, "max_cron_threads = 1") {
		t.Errorf("without a split the web tier should still run crons:\n%s", got)
	}
	// With a cron tier: crons happen in exactly one place, and it is not here.
	env.Spec.Workload = &doblurav1alpha1.WorkloadSplit{
		Cron: &doblurav1alpha1.CronTier{Replicas: 1, Threads: 2},
	}
	got := envOdooConf(env)
	if !strings.Contains(got, "max_cron_threads = 0") {
		t.Fatalf("with a cron tier the web tier MUST NOT run crons — they would run twice:\n%s", got)
	}
	// A cron tier scaled to zero is not a cron tier.
	env.Spec.Workload.Cron.Replicas = 0
	if got := envOdooConf(env); !strings.Contains(got, "max_cron_threads = 1") {
		t.Errorf("a cron tier at 0 replicas means nothing runs crons at all:\n%s", got)
	}
}

// ─────────────── the filestore ───────────────

func TestFilestoreIsEphemeralOnlyWhenNothingSaysOtherwise(t *testing.T) {
	env := &doblurav1alpha1.OdooEnvironment{
		ObjectMeta: metav1.ObjectMeta{Name: "e", Namespace: "d"},
		Spec: doblurav1alpha1.OdooEnvironmentSpec{
			Image:    "img",
			Database: doblurav1alpha1.DatabaseSpec{Host: "h", User: "u", PasswordSecret: "s"},
		},
	}
	// Default: an emptyDir, and now with a size limit. An unbounded one is charged
	// to the node and evicts the pod when the node fills — a failure that arrives
	// as "my environment disappeared" with nothing in its own events.
	v := filestoreVolume(env)
	if v.EmptyDir == nil {
		t.Fatal("the default filestore should still be an emptyDir")
	}
	if v.EmptyDir.SizeLimit == nil {
		t.Error("the emptyDir must be bounded, or it evicts the pod silently")
	}

	// A named claim is mounted, not created.
	env.Spec.Storage = &doblurav1alpha1.StorageSpec{
		Filestore: &doblurav1alpha1.FilestoreSpec{
			Mode: doblurav1alpha1.FilestorePVC, ClaimName: "theirs",
		},
	}
	v = filestoreVolume(env)
	if v.PersistentVolumeClaim == nil || v.PersistentVolumeClaim.ClaimName != "theirs" {
		t.Fatalf("a named claim must be mounted: %+v", v.VolumeSource)
	}
	if FilestoreClaim(env) != nil {
		t.Error("a claim somebody else manages must NOT be created here — its lifecycle is theirs")
	}

	// A size means the environment owns the claim.
	env.Spec.Storage.Filestore = &doblurav1alpha1.FilestoreSpec{
		Mode: doblurav1alpha1.FilestorePVC, Size: "20Gi",
	}
	v = filestoreVolume(env)
	if v.PersistentVolumeClaim == nil || v.PersistentVolumeClaim.ClaimName != "e-filestore" {
		t.Fatalf("an owned claim should be named after the environment: %+v", v.VolumeSource)
	}
	pvc := FilestoreClaim(env)
	if pvc == nil {
		t.Fatal("a size means a claim to create")
	}
	if pvc.Spec.AccessModes[0] != corev1.ReadWriteOnce {
		t.Errorf("without the RWX declaration the claim must be RWO: %v", pvc.Spec.AccessModes)
	}

	// And RWX only when declared, because it cannot be verified from here.
	env.Spec.Storage.Filestore.AccessModeReadWriteMany = true
	if FilestoreClaim(env).Spec.AccessModes[0] != corev1.ReadWriteMany {
		t.Error("the declaration must reach the claim")
	}
}

func TestDatabaseFilestoreNeedsNoVolumeAndSetsTheParameter(t *testing.T) {
	env := &doblurav1alpha1.OdooEnvironment{
		ObjectMeta: metav1.ObjectMeta{Name: "e", Namespace: "d"},
		Spec: doblurav1alpha1.OdooEnvironmentSpec{
			Image:    "img",
			Database: doblurav1alpha1.DatabaseSpec{Host: "h", User: "u", PasswordSecret: "s"},
			Storage: &doblurav1alpha1.StorageSpec{
				Filestore: &doblurav1alpha1.FilestoreSpec{Mode: doblurav1alpha1.FilestoreDatabase},
			},
		},
	}
	// No PVC: there is nothing to persist, which is the whole point.
	if v := filestoreVolume(env); v.PersistentVolumeClaim != nil {
		t.Error("Database mode must not claim a volume")
	}
	if FilestoreClaim(env) != nil {
		t.Error("Database mode must not create a PVC")
	}
	// The parameter is what makes Odoo write to db_datas, and it must be set in the
	// DATABASE, not in odoo.conf: it has to travel with the dump.
	got := envHardenScript(env)
	if !strings.Contains(got, "ir_attachment.location") || !strings.Contains(got, "'db'") {
		t.Fatalf("the hardening step must set ir_attachment.location = db:\n%s", got)
	}
	if !strings.Contains(got, "force_storage") {
		t.Error("existing attachments must be migrated, or they stay on a disk that is about to vanish")
	}
	// And it must NOT be set when the mode is anything else, or a PVC-backed
	// environment silently starts filling Postgres with blobs.
	env.Spec.Storage.Filestore.Mode = doblurav1alpha1.FilestorePVC
	env.Spec.Storage.Filestore.ClaimName = "fs"
	if strings.Contains(envHardenScript(env), "ir_attachment.location") {
		t.Error("a PVC-backed filestore must not be redirected into the database")
	}
}
