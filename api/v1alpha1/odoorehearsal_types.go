// SPDX-License-Identifier: Apache-2.0
// Copyright (C) 2026 Antoni Romera

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// AcknowledgementUnsafe is the literal value you must type to disable
// neutralization. It is deliberately long and awkward: nobody types it by
// accident, or "just temporarily to test something".
const AcknowledgementUnsafe = "i-accept-this-can-send-real-emails-and-charge-real-cards"

// ─────────────────────────── Migration ───────────────────────────

// MigrationEngine selects how the module update is executed.
// +kubebuilder:validation:Enum=ClickOdooUpdate;OdooUpdateAll;Marabunta
type MigrationEngine string

const (
	// EngineClickOdooUpdate updates only the modules whose checksum changed.
	// On a large database the difference against `-u all` is hours, not
	// minutes.
	EngineClickOdooUpdate MigrationEngine = "ClickOdooUpdate"

	// EngineOdooUpdateAll runs `odoo -u all`. Slow and blunt, but it is the
	// baseline to compare against: if something only breaks with the selective
	// engine, the problem is the checksum, not the migration.
	EngineOdooUpdateAll MigrationEngine = "OdooUpdateAll"

	// EngineMarabunta runs the versioned migrations declared in the
	// migration.yml that ships inside the image.
	EngineMarabunta MigrationEngine = "Marabunta"
)

// MigrationSpec describes how to migrate.
type MigrationSpec struct {
	// +kubebuilder:default=ClickOdooUpdate
	// +optional
	Engine MigrationEngine `json:"engine,omitempty"`

	// Modules restricts the update to these modules. Empty means whatever the
	// engine decides.
	// +optional
	Modules []string `json:"modules,omitempty"`

	// ExtraArgs is appended to the command. An escape hatch: if you need it
	// often, a field is missing from this API.
	// +optional
	ExtraArgs []string `json:"extraArgs,omitempty"`
}

// ─────────────────────────── Budget ───────────────────────────

// Budget is what conceptually separates Doblura from a plain timeout.
//
// A timeout is infrastructure hygiene: it kills something that hung. A budget
// is a business assertion: if the `-u` takes longer than your maintenance
// window, the release IS NOT VIABLE even though it technically worked.
// Exceeding the budget FAILS the rehearsal, it does not abort it.
//
// That is why the duration is a result in the status, not merely a stop
// condition.
type Budget struct {
	// MaxUpgradeDuration is the longest acceptable migration duration. Compare
	// it against your real maintenance window, not against your patience.
	// +optional
	MaxUpgradeDuration *metav1.Duration `json:"maxUpgradeDuration,omitempty"`

	// HardTimeout kills the rehearsal. This one really is hygiene: it stops a
	// stuck rehearsal from occupying the cluster forever.
	// +kubebuilder:default="6h"
	// +optional
	HardTimeout *metav1.Duration `json:"hardTimeout,omitempty"`
}

// ─────────────────────────── Assertions ───────────────────────────

// Assertions is what must hold true after migrating.
//
// That the migration finishes without an exception is implicit and is not
// declared: if it fails, the rehearsal fails. This is about what comes after.
type Assertions struct {
	// ModelCounts checks that critical models are still queryable after the
	// migration. It sounds trivial and it catches serious things: a `-u` can
	// exit 0 and leave a table unreachable.
	// +optional
	ModelCounts []ModelCountAssertion `json:"modelCounts,omitempty"`

	// Custom runs your own container against the migrated database. It gets
	// the connection through environment variables (PGHOST, PGDATABASE, ...)
	// and the composed odoo.conf under /etc/doblura. Exit code 0 means pass.
	//
	// This is a minimal struct and deliberately not a corev1.PodSpec:
	// embedding the full PodSpec pushed the CRD past the 262 KB annotation
	// limit and `kubectl apply` rejected it. It also exposes intent instead of
	// configuration, which is what a CRD should do.
	// +optional
	Custom *CustomAssertion `json:"custom,omitempty"`
}

// CustomAssertion is a container that decides whether the rehearsal passes.
type CustomAssertion struct {
	// Image defaults to the rehearsal's own image, which already carries Odoo
	// and the mounted addons. That is usually what you want.
	// +optional
	Image string `json:"image,omitempty"`

	// Command replaces the entrypoint.
	// +optional
	Command []string `json:"command,omitempty"`

	// +optional
	Args []string `json:"args,omitempty"`

	// Env adds variables. The database connection is already injected.
	// +optional
	Env map[string]string `json:"env,omitempty"`
}

// ModelCountAssertion asserts that a model holds records.
type ModelCountAssertion struct {
	// Model is the model name, for example account.move.
	// +kubebuilder:validation:MinLength=1
	Model string `json:"model"`

	// MinCount is the minimum expected record count. The default of 0 already
	// checks the thing that matters: that the model can be queried without
	// blowing up.
	// +kubebuilder:default=0
	// +optional
	MinCount int64 `json:"minCount,omitempty"`
}

// ─────────────────────────── Size and retention ───────────────────────────

// Size expresses intent, not resources. The translation to cpu/memory lives in
// the platform (internal/controller/sizes.go) and can change without anyone
// editing their manifests.
// +kubebuilder:validation:Enum=small;medium;large
type Size string

const (
	SizeSmall  Size = "small"
	SizeMedium Size = "medium"
	SizeLarge  Size = "large"
)

// RetentionPolicy decides what happens to the scratch database when the
// rehearsal ends.
// +kubebuilder:validation:Enum=Always;OnFailure;Never
type RetentionPolicy string

const (
	// RetainOnFailure keeps the database only when the rehearsal failed, so you
	// can go in and look. It is the default because it is what you want 95% of
	// the time: nothing to investigate on success, and on failure you need the
	// crime scene.
	RetainOnFailure RetentionPolicy = "OnFailure"
	RetainAlways    RetentionPolicy = "Always"
	RetainNever     RetentionPolicy = "Never"
)

// ─────────────────────────── Spec ───────────────────────────

// OdooRehearsalSpec rehearses a migration before it becomes irreversible.
type OdooRehearsalSpec struct {
	// Image is the candidate artifact: the exact image that would go to
	// production. Use a digest, not a moving tag, or the rehearsal proves
	// nothing.
	// +kubebuilder:validation:MinLength=1
	Image string `json:"image"`

	// Snapshot is where the data comes from and how to restore it.
	Snapshot SnapshotSpec `json:"snapshot"`

	// Addons declares where the modules come from. This matters in a
	// rehearsal, it is not decoration: click-odoo-update decides what to update
	// from the checksum of the modules it SEES on the addons path. If the
	// rehearsal's addons path is not production's, you are rehearsing a
	// different migration.
	// +optional
	Addons AddonsSpec `json:"addons,omitempty"`

	// Database is the connection to the Postgres server where the scratch
	// database will be created. The user needs CREATEDB.
	Database DatabaseSpec `json:"database"`

	// +optional
	Migration MigrationSpec `json:"migration,omitempty"`

	// +optional
	Assertions Assertions `json:"assertions,omitempty"`

	// +optional
	Budget *Budget `json:"budget,omitempty"`

	// +kubebuilder:default=medium
	// +optional
	Size Size `json:"size,omitempty"`

	// +kubebuilder:default=OnFailure
	// +optional
	Retain RetentionPolicy `json:"retain,omitempty"`
}

// DatabaseSpec is where to create the rehearsal's scratch database.
type DatabaseSpec struct {
	// Host of the Postgres server.
	// +kubebuilder:validation:MinLength=1
	Host string `json:"host"`

	// +kubebuilder:default=5432
	// +optional
	Port int32 `json:"port,omitempty"`

	// +kubebuilder:validation:MinLength=1
	User string `json:"user"`

	// PasswordSecret is the Secret holding the "password" key.
	// +kubebuilder:validation:MinLength=1
	PasswordSecret string `json:"passwordSecret"`
}

// ─────────────────────────── Status ───────────────────────────

// RehearsalPhase summarises the state in one word, for `kubectl get`.
// +kubebuilder:validation:Enum=Pending;Restoring;Migrating;Asserting;Succeeded;Failed
type RehearsalPhase string

const (
	PhasePending   RehearsalPhase = "Pending"
	PhaseRestoring RehearsalPhase = "Restoring"
	PhaseMigrating RehearsalPhase = "Migrating"
	PhaseAsserting RehearsalPhase = "Asserting"
	PhaseSucceeded RehearsalPhase = "Succeeded"
	PhaseFailed    RehearsalPhase = "Failed"
)

// Condition types. The standard convention, manipulated through
// meta.SetStatusCondition.
const (
	ConditionRestored  = "Restored"
	ConditionMigrated  = "Migrated"
	ConditionAsserted  = "Asserted"
	ConditionSucceeded = "Succeeded"
	// ConditionWithinBudget is deliberately separate from Migrated: a migration
	// can finish cleanly AND still not fit the window. Those are two distinct
	// facts and you want to tell them apart at a glance.
	ConditionWithinBudget = "WithinBudget"
)

// OdooRehearsalStatus is written by the controller only.
type OdooRehearsalStatus struct {
	// ObservedGeneration is the spec generation this status reflects. Without
	// it you cannot know whether what you are reading corresponds to what you
	// asked for.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// +optional
	Phase RehearsalPhase `json:"phase,omitempty"`

	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// UpgradeDuration is THE result of the rehearsal. It is not telemetry: it
	// is the number you plan the maintenance window with.
	// +optional
	UpgradeDuration *metav1.Duration `json:"upgradeDuration,omitempty"`

	// +optional
	StartedAt *metav1.Time `json:"startedAt,omitempty"`
	// +optional
	FinishedAt *metav1.Time `json:"finishedAt,omitempty"`

	// DatabaseName is the scratch database that was created. Whether it
	// survives depends on spec.retain.
	// +optional
	DatabaseName string `json:"databaseName,omitempty"`

	// ModelCounts are the observed counts, in assertion order. They are useful
	// even when the rehearsal passes: comparing them across runs surfaces data
	// loss that no single assertion catches.
	// +optional
	ModelCounts []ModelCountResult `json:"modelCounts,omitempty"`

	// Message is the human-readable summary of the outcome.
	// +optional
	Message string `json:"message,omitempty"`
}

// ModelCountResult is an observed count.
type ModelCountResult struct {
	Model string `json:"model"`
	Count int64  `json:"count"`
	// +optional
	Passed bool `json:"passed,omitempty"`
}

// ─────────────────────────── Root ───────────────────────────

// OdooRehearsal rehearses an Odoo migration against an anonymized copy of
// production, times it, and decides whether the release is viable.
//
// The use case: an Odoo `-u` alters the database schema and has no downgrade.
// The only way to know whether a migration works is to run it against the real
// data before running it against the real data.
//
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=rehearsal;odr
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Upgrade",type=string,JSONPath=`.status.upgradeDuration`,description="How long the migration took"
// +kubebuilder:printcolumn:name="Budget",type=string,JSONPath=`.spec.budget.maxUpgradeDuration`,description="Acceptable window"
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
type OdooRehearsal struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   OdooRehearsalSpec   `json:"spec,omitempty"`
	Status OdooRehearsalStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// OdooRehearsalList is a list of OdooRehearsal.
type OdooRehearsalList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []OdooRehearsal `json:"items"`
}

func init() {
	SchemeBuilder.Register(&OdooRehearsal{}, &OdooRehearsalList{})
}
