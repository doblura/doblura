// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Antoni Romera

package controller

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	doblurav1alpha1 "github.com/doblura/doblura/api/v1alpha1"
)

// The detail that makes all of this work: --exclude-table-data keeps the table
// and omits the rows. --exclude-table removes it entirely and Odoo's ORM will
// not start against the resulting schema.
func TestUsesExcludeTableDataAndNeverExcludeTable(t *testing.T) {
	yes := true
	s := &doblurav1alpha1.OdooSnapshotSpec{
		Truncate: doblurav1alpha1.TruncateSpec{Preset: &yes},
	}
	cfg := greenmaskConfig(s, "work")

	// Parsed, not grepped. The substring version of this test passed for weeks
	// against a config where duplicate keys discarded all but one table.
	var parsed map[string]any
	if err := yaml.Unmarshal([]byte(cfg), &parsed); err != nil {
		t.Fatalf("invalid YAML: %v", err)
	}
	opts := parsed["dump"].(map[string]any)["pg_dump_options"].(map[string]any)
	if _, wrong := opts["exclude-table"]; wrong {
		t.Error("NEVER bare exclude-table: it removes the table and the restore leaves a schema Odoo cannot start against")
	}
	excluded, ok := opts["exclude-table-data"].([]any)
	if !ok {
		t.Fatalf("exclude-table-data must be a list, got %T", opts["exclude-table-data"])
	}
	if len(excluded) != len(s.TablesToTruncate()) {
		t.Errorf("%d tables declared, %d in the config", len(s.TablesToTruncate()), len(excluded))
	}
}

// The Go zero value and the declared CRD default have to agree. Where they do
// not, the Go one wins on every path where defaulting has not run — and that is
// the path with no test.
func TestTruncateDefaultsOffInGoToo(t *testing.T) {
	// A spec with nothing declared, as if built in code rather than admitted.
	s := &doblurav1alpha1.OdooSnapshotSpec{}
	if got := s.TablesToTruncate(); len(got) != 0 {
		t.Errorf("an undeclared truncate must be OFF, matching +kubebuilder:default=false; got %d tables: %v", len(got), got)
	}
	cfg := greenmaskConfig(s, "work")
	if strings.Contains(cfg, "exclude-table-data") {
		t.Error("no truncation declared, so the config must not exclude any table data")
	}
}

// Deterministic or useless: if the same customer comes out different in every
// dump, QA cannot reproduce last week's bug.
func TestEveryTransformerIsDeterministic(t *testing.T) {
	s := &doblurav1alpha1.OdooSnapshotSpec{}
	cfg := greenmaskConfig(s, "work")

	// Every data-generating transformer must carry engine: hash.
	generators := strings.Count(cfg, "- name: \"Random")
	hashes := strings.Count(cfg, "engine: \"hash\"")
	if hashes < generators {
		t.Errorf("there are %d Random* transformers and only %d with engine hash", generators, hashes)
	}
	if strings.Contains(cfg, "engine: \"random\"") {
		t.Error("no transformer may use the random engine")
	}
}

// The YAML must be stable across reconciliations or the ConfigMap changes on its
// own and triggers phantom rollouts.
func TestTheConfigIsStable(t *testing.T) {
	s := &doblurav1alpha1.OdooSnapshotSpec{
		Mask: doblurav1alpha1.MaskSpec{Rules: []doblurav1alpha1.MaskRule{
			{Table: "z_table", Column: "c", Kind: doblurav1alpha1.MaskHash},
			{Table: "a_table", Column: "c", Kind: doblurav1alpha1.MaskHash},
		}},
	}
	first := greenmaskConfig(s, "t")
	for i := 0; i < 20; i++ {
		if greenmaskConfig(s, "t") != first {
			t.Fatal("greenmaskConfig is not deterministic across calls")
		}
	}
	// And ordered: a_table before z_table.
	if strings.Index(first, "a_table") > strings.Index(first, "z_table") {
		t.Error("tables must come out sorted")
	}
}

// Neutralizing comes BEFORE cleaning: if the pod dies halfway, the database
// cannot be left with production mail servers active.
func TestNeutralizeCutsMailAndCronsAndIsIdempotent(t *testing.T) {
	s := neutralizeScript("work", "/etc/doblura/odoo.conf")
	for _, want := range []string{"neutralize -d", "ir_mail_server SET active = false", "ir_cron SET active = false"} {
		if !strings.Contains(s, want) {
			t.Errorf("%q is missing from the neutralization script", want)
		}
	}
	// The SQL fallback must be idempotent: UPDATEs to false, not toggles.
	if strings.Contains(s, "NOT active") {
		t.Error("the fallback must set active=false, not invert it")
	}
}

// The SQL engine is deterministic too, and it does not rewrite already-null rows.
func TestSQLIsDeterministicAndSkipsNullRows(t *testing.T) {
	s := &doblurav1alpha1.OdooSnapshotSpec{}
	script := sqlMaskScript(s, "work")

	if !strings.Contains(script, "md5(") {
		t.Error("the SQL engine must derive from the original value to be deterministic")
	}
	if !strings.Contains(script, "IS NOT NULL") {
		t.Error("it must avoid rewriting already-null rows: on large tables that is hours")
	}
	if !strings.Contains(script, "BEGIN;") || !strings.Contains(script, "COMMIT;") {
		t.Error("all in one transaction: a half-applied masking is worse than none")
	}
	if !strings.Contains(script, "res_users SET password") {
		t.Error("passwords are reset by default")
	}
}

// A value containing a single quote must not break the generated SQL.
func TestSQLEscapesQuotes(t *testing.T) {
	r := doblurav1alpha1.MaskRule{
		Table: "t", Column: "c", Kind: doblurav1alpha1.MaskFixed, Value: "O'Brien",
	}
	got := sqlForRule(r)
	if !strings.Contains(got, "'O''Brien'") {
		t.Errorf("the quote must be escaped, got: %s", got)
	}
}

// The generated config has to be VALID YAML, not merely contain the right
// substrings. pg_dump_options is a map, and emitting exclude-table-data once per
// table produced duplicate keys: all but the last silently discarded, so exactly
// one table would have been truncated. Every Go assertion passed anyway, because
// they only checked that the string appeared somewhere.
func TestGreenmaskConfigIsValidYAML(t *testing.T) {
	yes := true
	s := &doblurav1alpha1.OdooSnapshotSpec{
		Truncate: doblurav1alpha1.TruncateSpec{Preset: &yes},
		Subset: &doblurav1alpha1.SubsetSpec{
			Companies:                   []string{"Acme ES", "Acme PT"},
			AcknowledgeSharedMasterData: doblurav1alpha1.AckSharedMasterData,
		},
	}
	cfg := greenmaskConfig(s, "work")

	var parsed map[string]any
	if err := yaml.Unmarshal([]byte(cfg), &parsed); err != nil {
		t.Fatalf("the generated config is not valid YAML: %v\n%s", err, cfg)
	}

	dump, ok := parsed["dump"].(map[string]any)
	if !ok {
		t.Fatal("no dump section")
	}
	opts, ok := dump["pg_dump_options"].(map[string]any)
	if !ok {
		t.Fatal("no pg_dump_options")
	}

	// Every truncated table must survive the round-trip, which is exactly what
	// duplicate keys destroyed.
	excluded, ok := opts["exclude-table-data"].([]any)
	if !ok {
		t.Fatalf("exclude-table-data must be a list, got %T", opts["exclude-table-data"])
	}
	if len(excluded) != len(s.TablesToTruncate()) {
		t.Errorf("%d tables declared, %d survived parsing", len(s.TablesToTruncate()), len(excluded))
	}

	// And a version must not be pinned into pg_bin_path: the Odoo 19 image ships
	// the pg18 client in /usr/bin.
	if common, ok := parsed["common"].(map[string]any); ok {
		if p, exists := common["pg_bin_path"]; exists {
			t.Errorf("pg_bin_path must not be pinned, got %v", p)
		}
	}
}
