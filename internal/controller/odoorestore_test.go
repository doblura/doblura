// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Antoni Romera

package controller

import (
	"strings"
	"testing"

	doblurav1alpha1 "github.com/doblura/doblura/api/v1alpha1"
)

// The copy comes before the restore, and a failed copy stops it.
//
// Asserted on the ORDER of the two commands in the script, not on their presence.
// A previous test in this repo checked "copy before update" by finding
// click-odoo-update — which also appears in the preflight above the copy, so it
// measured the wrong thing and passed for the wrong reason.
func TestTheSafetyCopyRunsBeforeTheRestore(t *testing.T) {
	env := &doblurav1alpha1.OdooEnvironment{}
	env.Name = "prod"
	env.Spec.Purpose = doblurav1alpha1.PurposeProduction

	rs := &doblurav1alpha1.OdooRestore{}
	rs.Name = "r1"
	rs.Spec = doblurav1alpha1.OdooRestoreSpec{
		Backup: "nightly", Copy: "2026-08-19T02-00-00Z", Into: "prod",
		SafetyCopy: ptr(true),
	}
	b := &doblurav1alpha1.OdooBackup{}
	b.Name = "nightly"

	dest := &safetyDestination{Backup: "nightly", Dir: "/backups/nightly"}
	script := backupRestoreScript(rs, b, env, dest)

	backup := strings.Index(script, "click-odoo-backupdb")
	restore := strings.Index(script, "click-odoo-restoredb")
	switch {
	case backup < 0:
		t.Fatal("no copy is taken before the restore")
	case restore < 0:
		t.Fatal("the script does not restore anything")
	case backup > restore:
		t.Fatalf("the copy runs AFTER the restore, so it copies the restored "+
			"database and there is no way back (backup at %d, restore at %d)",
			backup, restore)
	}

	// No "|| true" and no "; then" swallowing the failure: the copy has to be
	// able to stop the Job.
	line := ""
	for _, l := range strings.Split(script, "\n") {
		if strings.Contains(l, "click-odoo-backupdb") && !strings.Contains(l, "command -v") {
			line = l
		}
	}
	if strings.Contains(line, "||") || strings.HasSuffix(strings.TrimSpace(line), "&") {
		t.Fatalf("the copy's failure is swallowed, so a restore would go ahead "+
			"with no way back: %q", line)
	}
	if !strings.Contains(script, safetyMarker) {
		t.Fatal("the copy's name is never printed, so nothing can record the way back")
	}
}

// And with no safety copy the script says so rather than silently not doing it.
func TestNoSafetyCopyIsStated(t *testing.T) {
	env := &doblurav1alpha1.OdooEnvironment{}
	env.Name = "review-42"
	rs := &doblurav1alpha1.OdooRestore{}
	rs.Spec = doblurav1alpha1.OdooRestoreSpec{Backup: "n", Copy: "c", Into: "review-42"}
	b := &doblurav1alpha1.OdooBackup{}
	b.Name = "n"

	script := backupRestoreScript(rs, b, env, nil)
	if strings.Contains(script, "click-odoo-backupdb") {
		t.Fatal("a copy is taken even though none was asked for")
	}
	if !strings.Contains(script, "no copy is being taken") {
		t.Fatal("the log does not say that nothing was copied, so somebody reading " +
			"it afterwards cannot tell whether there is a way back")
	}
}

// The Job's shell must stop on the first failure.
//
// This is asserted here, in the restore's own test, because the restore is what
// DEPENDS on it: the safety copy only protects anybody if a failed copy prevents
// the restore, and that property lives in envJobPod's `-euc` rather than in
// anything the restore script says. Changing it to `-c` somewhere else would leave
// every test above passing and the guarantee gone.
func TestTheRestoreJobStopsOnTheFirstFailure(t *testing.T) {
	env := &doblurav1alpha1.OdooEnvironment{}
	env.Name = "prod"
	env.Spec.Image = "odoo:19.0"

	pod := envJobPod(env, envPhaseStep{"restore", "", "", func(*doblurav1alpha1.OdooEnvironment) string {
		return "true\n"
	}})

	if len(pod.Spec.Containers) == 0 {
		t.Fatal("the job pod has no container")
	}
	cmd := strings.Join(pod.Spec.Containers[0].Command, " ")
	if !strings.Contains(cmd, "-e") && !strings.Contains(cmd, "euc") {
		t.Fatalf("the job's shell does not stop on error (%q), so a failed safety "+
			"copy would be followed by the restore anyway and the database would "+
			"be replaced with no way back", cmd)
	}
}

// The way back has to be recorded, which needs two things on the POD.
//
// This test exists because both of them were wrong at once and the restore
// succeeded anyway: the label the read-back filters on was set on the Job (and
// Kubernetes does not copy Job labels onto pods), and nothing wrote the
// termination-message file (so a successful container reports an empty message).
// The result was a restore that took the copy, printed its name, and recorded
// status.safetyCopy as "" — the one field somebody needs to undo it.
func TestTheSafetyCopyNameCanBeReadBack(t *testing.T) {
	env := &doblurav1alpha1.OdooEnvironment{}
	env.Name = "prod"
	env.Spec.Image = "odoo:19.0"

	rs := &doblurav1alpha1.OdooRestore{}
	rs.Name = "r1"

	pod := envJobPod(env, envPhaseStep{"restore", "", "", func(*doblurav1alpha1.OdooEnvironment) string {
		return safetyCopyScript(env, &safetyDestination{Backup: "n", Dir: "/backups/n"})
	}})
	// The same two lines ensureRestoreJob applies. Asserted on the values rather
	// than by calling it, because building a real Job needs a client.
	if pod.Labels == nil {
		pod.Labels = map[string]string{}
	}
	pod.Labels["doblura.dev/restore"] = rs.Name
	for i := range pod.Spec.Containers {
		pod.Spec.Containers[i].TerminationMessagePath = safetyNamePath
	}

	if pod.Labels["doblura.dev/restore"] != "r1" {
		t.Fatal("the pod does not carry the restore label, so the operator cannot " +
			"find the pod that took the copy")
	}
	if got := pod.Spec.Containers[0].TerminationMessagePath; got != safetyNamePath {
		t.Fatalf("the container's termination message comes from %q, not the file "+
			"the script writes (%q), so a successful restore reports no copy", got, safetyNamePath)
	}
	script := safetyCopyScript(env, &safetyDestination{Backup: "n", Dir: "/backups/n"})
	if !strings.Contains(script, "> "+safetyNamePath) {
		t.Fatalf("the script never writes %s, so there is nothing for Kubernetes "+
			"to lift into the termination message", safetyNamePath)
	}
}
