// SPDX-License-Identifier: Apache-2.0
// Copyright (C) 2026 Antoni Romera

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ─────────────────────── Producing the anonymized copy ───────────────────────
//
// OdooRehearsal consumes an anonymized dump. OdooSnapshot produces it.
//
// The pipeline, and the order matters:
//
//	1. copy         a production copy into an isolated work database
//	2. NEUTRALIZE   cut outgoing mail, crons, payments and carriers
//	3. truncate     the volume tables (--exclude-table-data), optional
//	4. mask         the columns holding personal data
//	5. dump         to its destination
//	6. drop         the work database
//
// Neutralizing is step 2 and not step 4 on purpose: if the pod dies halfway
// through, the work database must not sit for even a second with production
// mail servers configured and reachable. Cut the outbound paths first, clean up
// afterwards.
//
// And neutralizing is NOT anonymizing: step 2 cuts the outbound paths, step 4
// erases the personal data. You need both, which is why they are two steps.

// MaskEngine is which tool does the masking.
//
// Same principle as the snapshots: generic first. Doblura does not implement an
// anonymization engine — it orchestrates one that already exists and is proven.
// Writing fake-data generators is a bottomless pit and it is not the
// interesting problem.
// +kubebuilder:validation:Enum=Greenmask;SQL;Custom
type MaskEngine string

const (
	// EngineGreenmask uses greenmask: a static Go binary, stateless, no
	// database extensions, and a drop-in replacement for pg_dump. Deterministic
	// through SHA-3 hashing, which is what keeps an anonymized environment
	// usable across dumps.
	//
	// Chosen over postgresql_anonymizer because that one is an EXTENSION: it has
	// to be installed in the production database, and its pg_dump_anon is
	// roughly twice as slow. Touching production in order to be able to
	// anonymize it is precisely what we want to avoid.
	EngineGreenmask MaskEngine = "Greenmask"

	// EngineSQL runs UPDATEs against the work database. No new dependencies,
	// full control, slower. Useful when your policy forbids third-party
	// binaries, or for rules no transformer covers.
	EngineSQL MaskEngine = "SQL"

	// EngineCustom runs your container against the work database. The same
	// contract as always: do your thing and exit 0.
	EngineCustom MaskEngine = "Custom"
)

// SourceDatabase is where the production copy comes from.
//
// +kubebuilder:validation:XValidation:rule="!has(self.live) || self.live == false || self.acknowledgeProductionRead == 'i-accept-reading-from-production-and-the-load-it-causes'",message="reading from the live production database requires acknowledgeProductionRead set to its literal value; use a replica or an existing backup if you can"
type SourceDatabase struct {
	// +kubebuilder:validation:MinLength=1
	Host string `json:"host"`

	// +kubebuilder:default=5432
	// +optional
	Port int32 `json:"port,omitempty"`

	// +kubebuilder:validation:MinLength=1
	User string `json:"user"`

	// +kubebuilder:validation:MinLength=1
	PasswordSecret string `json:"passwordSecret"`

	// Name is the source database.
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`

	// Live declares that Host points at the live PRODUCTION database, not a
	// replica and not a backup.
	//
	// A pg_dump is read-only and corrupts nothing, but it evicts the page cache
	// and competes for I/O for the whole duration of the dump. On a large
	// database that is noticeable in production.
	//
	// Preference, in order: an existing backup, a read replica, and only if
	// there is no alternative, production with this flag and inside a window.
	// +optional
	Live *bool `json:"live,omitempty"`

	// AcknowledgeProductionRead must be exactly
	// "i-accept-reading-from-production-and-the-load-it-causes" when Live is
	// true.
	// +optional
	AcknowledgeProductionRead string `json:"acknowledgeProductionRead,omitempty"`
}

// WorkDatabase is where the intermediate work database is created.
//
// It must be a DIFFERENT Postgres server from production, or at the very least a
// different user with CREATEDB. A full un-anonymized copy is restored and
// manipulated here: this is not a place to share a server with production.
type WorkDatabase struct {
	// +kubebuilder:validation:MinLength=1
	Host string `json:"host"`
	// +kubebuilder:default=5432
	// +optional
	Port int32 `json:"port,omitempty"`
	// +kubebuilder:validation:MinLength=1
	User string `json:"user"`
	// +kubebuilder:validation:MinLength=1
	PasswordSecret string `json:"passwordSecret"`
}

// TruncateSpec says which tables get emptied.
//
// DISABLED BY DEFAULT, and that is the single most important design decision in
// this type.
//
// The point of anonymizing is to lose sensitive information, NOT to lose rows.
// Those are different things, and conflating them ruins what the copy is for:
//
//   - Masking keeps every row, the volume, the relations and the distributions,
//     and destroys the personal content. Full fidelity.
//   - Truncating destroys rows. And with them, exactly what you wanted to
//     measure: mail_message is one of the slowest tables to migrate, so a
//     rehearsal with an empty chatter gives you a duration that is not the real
//     one. And a test environment with no message history is useless for anyone
//     trying to test anything.
//
// Truncating is an optimization paid for in fidelity. Sometimes it is worth it
// — a 200 GB database on a 32 GB homelab — but it is a conscious decision, not a
// default. Turn it on and the result no longer predicts the real duration.
type TruncateSpec struct {
	// Preset applies the volume-table list of a standard Odoo: chatter,
	// notifications, bus, logs, audit trails and job queues.
	//
	// It is implemented with `--exclude-table-data`, which keeps the table, the
	// schema, the indexes and the constraints and omits only the rows. NOT with
	// `--exclude-table`, which removes the whole table and leaves a schema
	// Odoo's ORM does not recognise. That distinction is what makes this
	// possible at all.
	//
	// false by default: see the type comment. What you want to lose is sensitive
	// data, not rows.
	// +kubebuilder:default=false
	// +optional
	Preset *bool `json:"preset,omitempty"`

	// Extra adds tables to the list.
	// +optional
	Extra []string `json:"extra,omitempty"`

	// Keep removes tables from the preset list, if you need one of them.
	// +optional
	Keep []string `json:"keep,omitempty"`
}

// MaskSpec says which columns get masked and with what.
type MaskSpec struct {
	// +kubebuilder:default=Greenmask
	// +optional
	Engine MaskEngine `json:"engine,omitempty"`

	// Preset applies the personal-data ruleset of a standard Odoo: contacts,
	// chatter, bank accounts, employees and CRM.
	// +kubebuilder:default=true
	// +optional
	Preset *bool `json:"preset,omitempty"`

	// Rules add to or override the preset. A rule on the same table+column
	// replaces the preset's.
	// +optional
	Rules []MaskRule `json:"rules,omitempty"`

	// ResetPasswords assigns the same known password to every user.
	//
	// true by default: leaving production password hashes in place is a leak,
	// and randomizing them leaves an environment nobody can log into, which is
	// the same problem by another route.
	//
	// For a publicly exposed environment this is not enough — see
	// OdooEnvironment.security.randomizeUserPasswords.
	// +kubebuilder:default=true
	// +optional
	ResetPasswords *bool `json:"resetPasswords,omitempty"`

	// MaskUserLogins additionally masks res_users.login.
	//
	// false by default, and deliberately so: change the logins and NOBODY can
	// log in with their usual account to validate the environment by hand. Only
	// turn it on if your logins are personal email addresses, and prepare a
	// technical account with a known login.
	// +optional
	MaskUserLogins *bool `json:"maskUserLogins,omitempty"`

	// CustomImage is the image used when Engine is Custom.
	// +optional
	CustomImage string `json:"customImage,omitempty"`
	// +optional
	CustomCommand []string `json:"customCommand,omitempty"`
}

// MaskRule masks one column.
type MaskRule struct {
	// +kubebuilder:validation:MinLength=1
	Table string `json:"table"`
	// +kubebuilder:validation:MinLength=1
	Column string   `json:"column"`
	Kind   MaskKind `json:"kind"`

	// Value is the constant used when Kind is Fixed.
	// +optional
	Value string `json:"value,omitempty"`
}

// SubsetSpec extracts one customer's data from a database that holds several.
//
// This is the quadrant that was missing. Masking removes the WHO — the personal
// data. Subsetting removes the WHOSE — the other customers' rows. They are
// orthogonal, and a dump you can hand to a customer needs both.
//
// Greenmask walks the foreign-key graph from an entry-point table and exports
// only the rows hanging off it, preserving referential integrity and handling
// circular references. Rooting that walk at res_company is what makes
// per-customer extraction possible at all.
//
// The caveat you own, and it does not go away
// ───────────────────────────────────────────
// Odoo shares master data between companies ON PURPOSE: contacts, products,
// users. So the subset for one customer still contains res_partner rows that are
// also another customer's contacts. It is literally the same row, and no filter
// separates it.
//
// Which means subsetting makes a Shared database *handoverable*, not *clean*.
// Doblura will let you do it and will keep saying so.
//
// +kubebuilder:validation:XValidation:rule="self.acknowledgeSharedMasterData == 'i-accept-odoo-shares-master-data-between-companies'",message="subsetting requires acknowledgeSharedMasterData set to its literal value: Odoo shares master data (contacts, products, users) between companies by design, so a per-company subset is handoverable but not a clean separation"
type SubsetSpec struct {
	// Companies are the res.company names to keep. Everything not reachable from
	// them is dropped.
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=64
	Companies []string `json:"companies"`

	// KeepGlobal are tables exported in full regardless of company, because they
	// are configuration rather than data and a subset without them produces a
	// database Odoo cannot start.
	//
	// The defaults are the registry and the access rights: strip those and the ORM
	// has nothing to boot from. They are a default rather than hardcoded because
	// your addons may add tables with the same character.
	// +kubebuilder:default={"ir_model","ir_model_fields","ir_model_data","ir_module_module","ir_ui_view","ir_config_parameter","res_groups","res_lang","res_currency","res_country"}
	// +optional
	KeepGlobal []string `json:"keepGlobal,omitempty"`

	// AcknowledgeSharedMasterData must be
	// "i-accept-odoo-shares-master-data-between-companies"
	// to confirm you have read the caveat above: a subset is not a clean
	// separation, because contacts, products and users are shared by design.
	//
	// It is required rather than advisory for the same reason the neutralize
	// acknowledgement is: this is the failure mode where somebody believes the
	// tool did something it cannot do.
	// +kubebuilder:validation:MinLength=1
	AcknowledgeSharedMasterData string `json:"acknowledgeSharedMasterData"`
}

// AckSharedMasterData is the literal value SubsetSpec requires.
const AckSharedMasterData = "i-accept-odoo-shares-master-data-between-companies"

// SnapshotDestination is where the anonymized dump is left.
//
// Same model as OdooRehearsal's read providers, inverted: generic first, with
// Custom as a first-class extension point.
//
// +kubebuilder:validation:XValidation:rule="self.type != 'Volume' || has(self.volume)",message="type Volume requires the volume field"
// +kubebuilder:validation:XValidation:rule="self.type != 'ObjectStore' || has(self.objectStore)",message="type ObjectStore requires the objectStore field"
// +kubebuilder:validation:XValidation:rule="self.type != 'Custom' || has(self.custom)",message="type Custom requires the custom field"
type SnapshotDestination struct {
	// +kubebuilder:validation:Enum=Volume;ObjectStore;Custom
	Type SnapshotProviderType `json:"type"`

	// +optional
	Volume *VolumeProvider `json:"volume,omitempty"`
	// +optional
	ObjectStore *ObjectStoreProvider `json:"objectStore,omitempty"`
	// +optional
	Custom *CustomProvider `json:"custom,omitempty"`
}

// OdooSnapshotSpec produce un dump anonimizado listo para un doblura.
type OdooSnapshotSpec struct {
	// Image is an Odoo image. It is needed to neutralize with the native command
	// and so the schema matches the source's.
	// +kubebuilder:validation:MinLength=1
	Image string `json:"image"`

	// Source is what gets copied.
	Source SourceDatabase `json:"source"`

	// Work is where the intermediate database is created.
	Work WorkDatabase `json:"work"`

	// Neutralize cuts outgoing mail, crons, payment providers and carriers.
	//
	// true by default, and here there is NO way to turn it off. OdooRehearsal
	// has the acknowledgement escape hatch because you sometimes rehearse
	// against data that was neutralized earlier. Here we are producing the
	// artifact others will consume: if it leaves un-neutralized, the damage
	// propagates to every environment that uses it.
	// +kubebuilder:default=true
	// +optional
	Neutralize *bool `json:"neutralize,omitempty"`

	// +optional
	Truncate TruncateSpec `json:"truncate,omitempty"`

	// +optional
	Mask MaskSpec `json:"mask,omitempty"`

	// Subset extracts only the declared companies. Omit it to dump everything,
	// which is correct for a single-tenant database and wrong for a shared one.
	// +optional
	Subset *SubsetSpec `json:"subset,omitempty"`

	// To is where the result is left.
	To SnapshotDestination `json:"to"`

	// Schedule in cron format. When set, a CronJob is created; otherwise it runs
	// once.
	//
	// Weekly is the sensible cadence: a week-old dump is perfectly fine for
	// rehearsing a migration, and daily is load for nothing.
	// +optional
	// +kubebuilder:validation:Pattern=`^(@(hourly|daily|weekly|monthly)|([-0-9*/,]+\s+){4}[-0-9*/,]+)$`
	Schedule string `json:"schedule,omitempty"`

	// +kubebuilder:default=medium
	// +optional
	Size Size `json:"size,omitempty"`

	// DenyEgress adds a NetworkPolicy stopping the pod from talking to anything
	// other than its Postgres and its destination.
	//
	// true by default: this pod holds a COMPLETE, un-anonymized copy of
	// production for several minutes. It is the most sensitive pod in the whole
	// cluster and it has no reason whatsoever to reach the internet.
	// +kubebuilder:default=true
	// +optional
	DenyEgress *bool `json:"denyEgress,omitempty"`
}

// SnapshotPhase summarises the state.
// +kubebuilder:validation:Enum=Pending;Copying;Neutralizing;Anonymizing;Dumping;Uploading;Succeeded;Failed
type SnapshotPhase string

const (
	SnapPending      SnapshotPhase = "Pending"
	SnapCopying      SnapshotPhase = "Copying"
	SnapNeutralizing SnapshotPhase = "Neutralizing"
	SnapAnonymizing  SnapshotPhase = "Anonymizing"
	SnapDumping      SnapshotPhase = "Dumping"
	SnapUploading    SnapshotPhase = "Uploading"
	SnapSucceeded    SnapshotPhase = "Succeeded"
	SnapFailed       SnapshotPhase = "Failed"
)

// OdooSnapshotStatus is written by the controller only.
type OdooSnapshotStatus struct {
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
	// +optional
	Phase SnapshotPhase `json:"phase,omitempty"`

	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// LastSuccessfulTime is when the last good dump was produced. It is the
	// operational number: a three-month-old dump makes every rehearsal lie, and
	// this is what gives it away.
	// +optional
	LastSuccessfulTime *metav1.Time `json:"lastSuccessfulTime,omitempty"`

	// SizeBytes of the resulting dump. Compared against production's size it
	// tells you whether truncation actually did anything.
	// +optional
	SizeBytes int64 `json:"sizeBytes,omitempty"`

	// TablesTruncated and ColumnsMasked are the effective counts. They serve as
	// evidence of diligence: how many personal-data columns were actually
	// handled in this particular dump.
	// +optional
	TablesTruncated int32 `json:"tablesTruncated,omitempty"`
	// +optional
	ColumnsMasked int32 `json:"columnsMasked,omitempty"`

	// +optional
	Message string `json:"message,omitempty"`
}

// OdooSnapshot produces an anonymized, neutralized dump of production, ready to
// feed an OdooRehearsal or an OdooEnvironment.
//
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=osnap
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Last",type=date,JSONPath=`.status.lastSuccessfulTime`,description="Last good dump"
// +kubebuilder:printcolumn:name="Size",type=integer,JSONPath=`.status.sizeBytes`
// +kubebuilder:printcolumn:name="Masked",type=integer,JSONPath=`.status.columnsMasked`,description="Masked columns"
// +kubebuilder:printcolumn:name="Subset",type=string,JSONPath=`.spec.subset.companies`,description="Companies extracted, empty means the whole database"
// +kubebuilder:printcolumn:name="Schedule",type=string,JSONPath=`.spec.schedule`
type OdooSnapshot struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   OdooSnapshotSpec   `json:"spec,omitempty"`
	Status OdooSnapshotStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// OdooSnapshotList is a list of OdooSnapshot.
type OdooSnapshotList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []OdooSnapshot `json:"items"`
}

func init() {
	SchemeBuilder.Register(&OdooSnapshot{}, &OdooSnapshotList{})
}

// ─────────────────────── Resolving the presets ───────────────────────

// TablesToTruncate resolves preset + extra − keep.
//
// It lives in the API next to the types and tests on its own: this is the code
// that decides which data disappears, and it should be reasonable about without
// reading the reconciler.
func (s *OdooSnapshotSpec) TablesToTruncate() []string {
	keep := map[string]bool{}
	for _, k := range s.Truncate.Keep {
		keep[k] = true
	}

	seen := map[string]bool{}
	var out []string
	add := func(t string) {
		if keep[t] || seen[t] {
			return
		}
		// Tables in TruncateNeverOdoo are ignored even when asked for: emptying
		// one of them turns the rehearsal into a lie, and silently ignoring the
		// request beats producing a dump that looks valid and is not.
		if _, never := TruncateNeverOdoo[t]; never {
			return
		}
		seen[t] = true
		out = append(out, t)
	}

	// nil means OFF, matching the +kubebuilder:default=false on the field.
	//
	// It used to mean ON, and the two disagreed in the dangerous direction:
	// through the API server defaulting made it false and nothing was truncated,
	// but any spec built in Go without defaulting silently truncated fifteen
	// tables — including mail_message, which is exactly what makes a measured
	// migration duration stop predicting the real window.
	//
	// When a field's Go zero value and its declared default disagree, the Go one
	// wins wherever defaulting has not run, and that is the code path nobody
	// tests. They have to agree.
	if s.Truncate.Preset != nil && *s.Truncate.Preset {
		for _, t := range TruncatePresetOdoo {
			add(t)
		}
	}
	for _, t := range s.Truncate.Extra {
		add(t)
	}
	return out
}

// RulesToApply resolves preset + logins + user-supplied rules.
//
// A user rule on the same table+column replaces the preset's, so you can tune
// one column without having to disable the whole preset and rewrite it.
func (s *OdooSnapshotSpec) RulesToApply() []MaskRule {
	idx := map[string]int{}
	var out []MaskRule
	put := func(r MaskRule) {
		k := r.Table + "." + r.Column
		if i, ok := idx[k]; ok {
			out[i] = r
			return
		}
		idx[k] = len(out)
		out = append(out, r)
	}

	if s.Mask.Preset == nil || *s.Mask.Preset {
		for _, p := range MaskPresetOdoo {
			put(MaskRule{Table: p.Table, Column: p.Column, Kind: p.Kind})
		}
		if s.Mask.MaskUserLogins != nil && *s.Mask.MaskUserLogins {
			for _, p := range MaskPresetUserLogins {
				put(MaskRule{Table: p.Table, Column: p.Column, Kind: p.Kind})
			}
		}
	}
	for _, r := range s.Mask.Rules {
		put(r)
	}
	return out
}
