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
	// Guarded on the real condition — attachments actually on disk — not on the
	// data type. A freshly initialised database has none, so it must not be made
	// to require click-odoo-contrib for nothing.
	if !strings.Contains(got, "store_fname IS NOT NULL") {
		t.Error("the migration must be guarded on whether anything is on disk")
	}
	if !strings.Contains(got, "force_storage") {
		t.Error("attachments that ARE on disk must still be moved")
	}
	if !strings.Contains(got, "nothing to move") {
		t.Error("the empty case must say so rather than failing")
	}
	// And it must NOT be set when the mode is anything else, or a PVC-backed
	// environment silently starts filling Postgres with blobs.
	env.Spec.Storage.Filestore.Mode = doblurav1alpha1.FilestorePVC
	env.Spec.Storage.Filestore.ClaimName = "fs"
	if strings.Contains(envHardenScript(env), "ir_attachment.location") {
		t.Error("a PVC-backed filestore must not be redirected into the database")
	}
}

func TestLocallyInitialisedDataIsNotMigrated(t *testing.T) {
	// A database this operator just created is already at the image's version.
	// Running the migrate phase on it does nothing except demand click-odoo-contrib
	// — which is how a Demo environment failed with "click-odoo-update: not found"
	// for a step it never needed. The engine field carries a default, so testing it
	// alone made an optional phase unconditional.
	r := &OdooEnvironmentReconciler{}
	for _, dt := range []doblurav1alpha1.EnvDataType{
		doblurav1alpha1.DataDemo, doblurav1alpha1.DataEmpty,
	} {
		env := &doblurav1alpha1.OdooEnvironment{Spec: doblurav1alpha1.OdooEnvironmentSpec{
			Data:      doblurav1alpha1.EnvData{Type: dt},
			Migration: doblurav1alpha1.MigrationSpec{Engine: doblurav1alpha1.EngineClickOdooUpdate},
		}}
		for _, s := range r.phasePipeline(env) {
			if s.name == "migrate" {
				t.Errorf("%s data must not run the migrate phase", dt)
			}
		}
	}
	// Data that came from somewhere else still does.
	env := &doblurav1alpha1.OdooEnvironment{Spec: doblurav1alpha1.OdooEnvironmentSpec{
		Data:      doblurav1alpha1.EnvData{Type: doblurav1alpha1.DataSnapshot},
		Migration: doblurav1alpha1.MigrationSpec{Engine: doblurav1alpha1.EngineClickOdooUpdate},
	}}
	var saw bool
	for _, s := range r.phasePipeline(env) {
		saw = saw || s.name == "migrate"
	}
	if !saw {
		t.Error("a snapshot-backed environment must still be migrated")
	}
}

func TestMigrateScriptNamesTheMissingRequirement(t *testing.T) {
	env := &doblurav1alpha1.OdooEnvironment{
		ObjectMeta: metav1.ObjectMeta{Name: "e", Namespace: "d"},
		Spec: doblurav1alpha1.OdooEnvironmentSpec{
			Image:     "odoo:18",
			Database:  doblurav1alpha1.DatabaseSpec{Host: "h", User: "u", PasswordSecret: "s"},
			Migration: doblurav1alpha1.MigrationSpec{Engine: doblurav1alpha1.EngineClickOdooUpdate},
		},
	}
	got := envMigrateScript(env)
	if !strings.Contains(got, "click-odoo-contrib") || !strings.Contains(got, "odoo:18") {
		t.Fatalf("the failure must name the requirement and the image:\n%s", got)
	}
	// odoo -u needs no such tool, so it gets no guard.
	env.Spec.Migration.Engine = doblurav1alpha1.EngineOdooUpdateAll
	if strings.Contains(envMigrateScript(env), "click-odoo-contrib") {
		t.Error("the odoo -u engine needs no click-odoo-contrib guard")
	}
}

// Hardening cuts INCOMING mail as well as outgoing.
//
// The outgoing half has always been there. The incoming half was not, while the
// snapshot pipeline's identical list did cut it — an asymmetry nothing pointed
// at. What it allowed is worse than the outgoing case: a copy of production
// polling the customer's real mailbox consumes the messages it reads, so mail
// meant for the real system lands in a copy nobody is watching and is gone from
// the inbox. There is no equivalent of the recipient who tells you.
func TestHardeningCutsIncomingMailToo(t *testing.T) {
	env := &doblurav1alpha1.OdooEnvironment{
		ObjectMeta: metav1.ObjectMeta{Name: "e", Namespace: "d"},
		Spec: doblurav1alpha1.OdooEnvironmentSpec{
			Image:    "img",
			Database: doblurav1alpha1.DatabaseSpec{Host: "h", User: "u", PasswordSecret: "s"},
		},
	}
	got := envHardenScript(env)
	for _, want := range []string{
		"UPDATE ir_mail_server SET active = false",
		"UPDATE fetchmail_server SET active = false",
		"UPDATE ir_cron SET active = false",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("hardening does not do this:\n  %s", want)
		}
	}
	// fetchmail is a module and may not be installed. Without the guard the whole
	// transaction fails and takes the two lines that matter most with it.
	if !strings.Contains(got, "to_regclass('fetchmail_server')") {
		t.Error("the fetchmail line must be guarded on the table existing")
	}
}
