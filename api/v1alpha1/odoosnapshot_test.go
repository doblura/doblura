// SPDX-License-Identifier: Apache-2.0
// Copyright (C) 2026 Antoni Romera

package v1alpha1

import (
	"strings"
	"testing"
)

// Tables in TruncateNeverOdoo are ignored EVEN WHEN asked for. Emptying
// ir_model_data or account_move turns the rehearsal into a lie, and ignoring the
// request beats producing a dump that looks good and is not.
func TestForbiddenTablesAreNeverTruncated(t *testing.T) {
	s := &OdooSnapshotSpec{
		Truncate: TruncateSpec{
			Extra: []string{"account_move", "ir_model_data", "ir_ui_view", "mi_tabla_propia"},
		},
	}
	got := s.TablesToTruncate()

	for _, forbidden := range []string{"account_move", "ir_model_data", "ir_ui_view"} {
		for _, g := range got {
			if g == forbidden {
				t.Errorf("%s must never be truncated: %s", forbidden, TruncateNeverOdoo[forbidden])
			}
		}
	}
	// And a table of your own does go through.
	var ownTable bool
	for _, g := range got {
		if g == "mi_tabla_propia" {
			ownTable = true
		}
	}
	if !ownTable {
		t.Error("a table of your own should be accepted")
	}
}

func TestTruncatePresetAndKeep(t *testing.T) {
	yes := true
	s := &OdooSnapshotSpec{Truncate: TruncateSpec{Preset: &yes, Keep: []string{"mail_followers"}}}
	got := s.TablesToTruncate()

	var hayMailMessage, hayFollowers bool
	for _, g := range got {
		if g == "mail_message" {
			hayMailMessage = true
		}
		if g == "mail_followers" {
			hayFollowers = true
		}
	}
	if !hayMailMessage {
		t.Error("the preset must include mail_message: it is suspect number one for size")
	}
	if hayFollowers {
		t.Error("keep must remove mail_followers from the preset")
	}

	// Without the preset, only what was declared explicitly.
	no := false
	s2 := &OdooSnapshotSpec{Truncate: TruncateSpec{Preset: &no, Extra: []string{"only_this"}}}
	if g := s2.TablesToTruncate(); len(g) != 1 || g[0] != "only_this" {
		t.Errorf("without the preset only [only_this] was expected, got %v", g)
	}
}

// A user rule on the same table+column replaces the preset's, so you can tune
// one column without disabling and rewriting the whole preset.
func TestAUserRuleReplacesThePresetOne(t *testing.T) {
	s := &OdooSnapshotSpec{
		Mask: MaskSpec{
			Rules: []MaskRule{{Table: "res_partner", Column: "name", Kind: MaskFixed, Value: "ANONIMO"}},
		},
	}
	var found int
	for _, r := range s.RulesToApply() {
		if r.Table == "res_partner" && r.Column == "name" {
			found++
			if r.Kind != MaskFixed || r.Value != "ANONIMO" {
				t.Errorf("the user rule must win, got %+v", r)
			}
		}
	}
	if found != 1 {
		t.Errorf("res_partner.name must appear ONCE, it appears %d times", found)
	}
}

// Logins are NOT masked by default: change them and nobody can log in to
// validate the environment, which makes the whole exercise pointless.
func TestLoginsAreNotMaskedByDefault(t *testing.T) {
	s := &OdooSnapshotSpec{}
	for _, r := range s.RulesToApply() {
		if r.Table == "res_users" && r.Column == "login" {
			t.Fatal("res_users.login must not be masked by default: it would lock everyone out")
		}
	}

	yes := true
	s2 := &OdooSnapshotSpec{Mask: MaskSpec{MaskUserLogins: &yes}}
	var hay bool
	for _, r := range s2.RulesToApply() {
		if r.Table == "res_users" && r.Column == "login" {
			hay = true
		}
	}
	if !hay {
		t.Error("with maskUserLogins it must appear")
	}
}

// The preset must cover the four personal-data families of a standard Odoo. If
// somebody deletes one by accident, this catches it.
func TestThePresetCoversThePIIFamilies(t *testing.T) {
	s := &OdooSnapshotSpec{}
	tablas := map[string]bool{}
	for _, r := range s.RulesToApply() {
		tablas[r.Table] = true
	}
	for _, expected := range []string{"res_partner", "res_partner_bank", "hr_employee", "crm_lead"} {
		if !tablas[expected] {
			t.Errorf("the preset must cover %s", expected)
		}
	}
	// And the contact email, the single most identifying field.
	var email bool
	for _, r := range s.RulesToApply() {
		if r.Table == "res_partner" && r.Column == "email" && r.Kind == MaskEmail {
			email = true
		}
	}
	if !email {
		t.Error("res_partner.email is indispensable in the preset")
	}
}

// The neutralization subcommand comes first, in every format that uses it.
//
// It did not, and Odoo takes its FIRST argument as the command: "odoo -c conf
// neutralize -d db" falls back to the server command and dies on a stray
// positional. In these two formats that sits inside an && chain, so the restore
// failed at its last step — which means neither has ever completed a restore
// with neutralization switched on. The default format neutralizes through
// click-odoo-restoredb and is the one the e2e runs, which is why nobody saw it.
func TestNeutralizationPutsTheSubcommandFirst(t *testing.T) {
	for _, f := range []SnapshotFormat{FormatPgDump, FormatPgPlain} {
		cmd := (&SnapshotSpec{Format: f}).RestoreCommand("db", "/etc/doblura/odoo.conf", "/src")
		if !strings.Contains(cmd, "odoo neutralize -c ") {
			t.Errorf("%s: %q", f, cmd)
		}
		if strings.Contains(cmd, "odoo -c ") {
			t.Errorf("%s puts the config flag before the subcommand, so Odoo never "+
				"sees the subcommand: %q", f, cmd)
		}
	}
}
