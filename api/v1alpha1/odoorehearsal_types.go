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
	// Modules checks WHICH modules the rehearsal actually exercised.
	//
	// The README already warns that if the rehearsal's addons PATH is not
	// production's, you are rehearsing a different migration. The installed SET is
	// the other half of that sentence and the one nothing checked: OpenUpgrade runs
	// a module's migration scripts only if that module is installed, so a rehearsal
	// against a database missing a module silently never exercises its migration —
	// and passes.
	// +optional
	Modules *ModuleAssertion `json:"modules,omitempty"`

	// +optional
	Custom *CustomAssertion `json:"custom,omitempty"`
}

// ModuleAssertion checks the installed module set.
//
// Declared rather than discovered, and that is a limitation worth naming: the
// operator cannot read production's module set, because nothing observes an
// OdooDatabase yet. So this is you writing down what the rehearsal is supposed to
// cover, and the rehearsal refusing to pass quietly when it does not.
//
// +kubebuilder:validation:XValidation:rule="has(self.installed) || has(self.minCount)",message="a modules assertion needs installed, minCount, or both; an empty one would pass unconditionally, which is worse than not declaring it"
type ModuleAssertion struct {
	// Installed lists modules that must be installed in the restored copy.
	//
	// Name them for the modules whose migration you actually care about — the ones
	// with data to transform. A rehearsal that passes without `account` installed
	// has told you nothing about the part that takes 96% of the time.
	// +kubebuilder:validation:MaxItems=256
	// +optional
	Installed []string `json:"installed,omitempty"`

	// MinCount is the smallest acceptable number of installed modules.
	//
	// A blunt instrument on purpose. It catches the failure that matters — a
	// snapshot restored into a database that came up with a fraction of the
	// modules — without anybody having to enumerate four hundred names.
	// +kubebuilder:validation:Minimum=1
	// +optional
	MinCount *int32 `json:"minCount,omitempty"`
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

	// ForTenant is the customer whose migration this rehearses.
	//
	// Optional, and the same field the other kinds carry. Without it a rehearsal
	// is an experiment that proves something about an image and about nobody in
	// particular — which is enough to answer "does this version migrate?" and not
	// enough to answer "has THIS customer's data been through it?". A staged
	// rollout across customers has to ask the second question, one customer at a
	// time, and it cannot without this.
	// +optional
	ForTenant string `json:"forTenant,omitempty"`

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

	// Proxy puts a connection pooler between Odoo and this server.
	// +optional
	Proxy *DatabaseProxySpec `json:"proxy,omitempty"`
}

// ─────────────── The connection proxy ───────────────
//
// The question this answers is not pooling. It is: can the database live outside
// the cluster without the workload holding the credential to it?
//
// Without a proxy the answer is no, and not by a little. PGPASSWORD is an
// environment variable on the Odoo container, so it is in `kubectl describe pod`,
// in any core dump, in the process environment of everything Odoo ever execs, and
// readable by anyone who can open a shell in that container. The host and port of
// the production database are next to it. An addon with a Python sandbox escape —
// which is a category, not a hypothetical — gets both.
//
// With the sidecar, the Secret is mounted into the PROXY container and nowhere
// else. Containers in one pod share a network namespace but NOT a filesystem, so
// Odoo connects to 127.0.0.1 with no password at all, and cannot read the file
// that holds the real one or learn the address it is being forwarded to.
//
// Two honest limits, because this is a boundary and not a wall:
//
//   - It stops the workload reading the credential. It does not stop anything
//     that can already reach the pod's ServiceAccount from asking the API server
//     for the Secret. Give environment pods a ServiceAccount with no secret
//     access, which is what they should have regardless.
//   - Anyone who can exec into the Odoo container can still USE the database
//     through the loopback socket. The credential is hidden; the access is not.
//     This buys you rotation without redeploying and a blast radius that stops at
//     one database, not confidentiality against someone already inside.

// DatabaseProxyMode selects the proxy topology.
// +kubebuilder:validation:Enum=None;Sidecar
type DatabaseProxyMode string

const (
	// ProxyNone connects Odoo straight to spec.database.host.
	ProxyNone DatabaseProxyMode = "None"
	// ProxySidecar runs the pooler in the same pod, on loopback.
	ProxySidecar DatabaseProxyMode = "Sidecar"
)

// DatabaseProxyPoolMode is pgbouncer's pool_mode.
// +kubebuilder:validation:Enum=Session;Transaction
type DatabaseProxyPoolMode string

const (
	PoolSession     DatabaseProxyPoolMode = "Session"
	PoolTransaction DatabaseProxyPoolMode = "Transaction"
)

// TransactionPoolingAck is the literal that acknowledges what Transaction costs.
const TransactionPoolingAck = "i-accept-transaction-pooling-breaks-the-odoo-bus"

// DatabaseProxySpec configures the pooler.
// +kubebuilder:validation:XValidation:rule="self.mode != 'Sidecar' || has(self.image)",message="proxy mode Sidecar needs an explicit image: there is no sensible default, because the pooler image is the one container in the pod that will hold your database credential and pinning a vendor tag on your behalf is not a decision this operator should make for you"
// +kubebuilder:validation:XValidation:rule="self.poolMode != 'Transaction' || (has(self.unsafeAcknowledgement) && self.unsafeAcknowledgement == 'i-accept-transaction-pooling-breaks-the-odoo-bus')",message="poolMode Transaction requires unsafeAcknowledgement set to its literal value: Odoo's bus registers with LISTEN, which is session state, and transaction pooling does not preserve it: the failure is that live notifications silently stop, and it appears under concurrency rather than in testing"
type DatabaseProxySpec struct {
	// +kubebuilder:default=None
	// +optional
	Mode DatabaseProxyMode `json:"mode,omitempty"`

	// Image is the pooler. pgbouncer is what the generated configuration speaks.
	// +kubebuilder:validation:MinLength=1
	// +optional
	Image string `json:"image,omitempty"`

	// PoolMode defaults to Session, and that default is the whole point.
	//
	// Session is the only mode that keeps a client on one server connection for
	// the life of the connection, and Odoo needs that: bus.py issues
	// `listen imbus` and then waits on the socket. Transaction pooling returns
	// the server connection to the pool the moment the transaction ends, so the
	// registration is on a backend the listener no longer holds.
	//
	// Everything else Odoo does is transaction-scoped and would be fine —
	// ir_cron serialises with FOR NO KEY UPDATE SKIP LOCKED, mail_thread uses
	// pg_try_advisory_xact_lock. The bus is the one exception, and it is enough.
	// +kubebuilder:default=Session
	// +optional
	PoolMode DatabaseProxyPoolMode `json:"poolMode,omitempty"`

	// UnsafeAcknowledgement gates Transaction. See the CEL message.
	// +optional
	UnsafeAcknowledgement string `json:"unsafeAcknowledgement,omitempty"`

	// +kubebuilder:validation:Minimum=1
	// +optional
	MaxClientConn *int32 `json:"maxClientConn,omitempty"`

	// +kubebuilder:validation:Minimum=1
	// +optional
	DefaultPoolSize *int32 `json:"defaultPoolSize,omitempty"`
}

// ProxyEnabled reports whether Odoo talks to the database over loopback.
func (d *DatabaseSpec) ProxyEnabled() bool {
	return d != nil && d.Proxy != nil && d.Proxy.Mode == ProxySidecar
}

// ConnectHost is the host the WORKLOAD connects to, which is not necessarily the
// host the administrator wrote down.
func (d *DatabaseSpec) ConnectHost() string {
	if d.ProxyEnabled() {
		return "127.0.0.1"
	}
	return d.Host
}

// ConnectPort mirrors ConnectHost. The proxy always listens on 5432, so nothing
// downstream — odoo.conf, psql, click-odoo — needs to know it is there.
func (d *DatabaseSpec) ConnectPort() int32 {
	if d.ProxyEnabled() {
		return ProxyListenPort
	}
	if d.Port == 0 {
		return 5432
	}
	return d.Port
}

// ProxyListenPort is where the sidecar listens on loopback.
const ProxyListenPort int32 = 5432

// PoolModeString is pgbouncer's spelling.
func (p *DatabaseProxySpec) PoolModeString() string {
	if p != nil && p.PoolMode == PoolTransaction {
		return "transaction"
	}
	return "session"
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
