// SPDX-License-Identifier: Apache-2.0
// Copyright (C) 2026 Antoni Romera

package v1alpha1

import (
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ─────────────── Environments with real data and public access ───────────────
//
// The unified type from the roadmap: two independent axes, four cases.
//
//	          data           lifecycle
//	PR        Empty/Demo     Hibernating
//	Rehearsal Snapshot       Ephemeral
//	Staging   Snapshot       Persistent
//	Prod      Live           Persistent
//
// Plus a third axis the others did not have: EXPOSURE. A test environment with
// real anonymized data and a public URL has the same attack surface as
// production, and therefore needs the same controls.
//
// The contradiction to settle before anything else
// ────────────────────────────────────────────────
// OdooSnapshot assigns the SAME known password to every user, and that is
// correct: a rehearsal is ephemeral, has no ingress, and an environment nobody
// can log into cannot be validated.
//
// That same dump served on a public URL means anyone logs in as administrator.
// The right decision in one context is catastrophic in the other, and no single
// default serves both.
//
// Which is why `exposure.public: true` REQUIRES
// `security.randomizeUserPasswords`, and the API server enforces it, not the
// documentation.

// EnvDataType is where the environment's data comes from.
// +kubebuilder:validation:Enum=Empty;Demo;Snapshot;Live
type EnvDataType string

const (
	// DataEmpty creates the database with the modules installed and nothing in it.
	DataEmpty EnvDataType = "Empty"
	// DataDemo installs Odoo's demo data. For pull-request environments.
	DataDemo EnvDataType = "Demo"
	// DataSnapshot restores an anonymized dump produced by OdooSnapshot.
	DataSnapshot EnvDataType = "Snapshot"
	// DataLive points at an existing database. Production.
	DataLive EnvDataType = "Live"
)

// EnvLifecycleType is how long the environment lives.
// +kubebuilder:validation:Enum=Ephemeral;Hibernating;Persistent
type EnvLifecycleType string

const (
	// LifecycleEphemeral is born, used, and dies on a TTL.
	LifecycleEphemeral EnvLifecycleType = "Ephemeral"
	// LifecycleHibernating sits at zero replicas and wakes on demand. It is what
	// makes hundreds of pull-request environments viable.
	LifecycleHibernating EnvLifecycleType = "Hibernating"
	// LifecyclePersistent is always up.
	LifecyclePersistent EnvLifecycleType = "Persistent"
)

// EnvAuthType is who can reach the environment.
// +kubebuilder:validation:Enum=None;BasicAuth;ForwardAuth
type EnvAuthType string

const (
	// IngressAuthNone leaves the environment open. Only valid when not public.
	IngressAuthNone EnvAuthType = "None"
	// IngressAuthBasic puts basic auth on the ingress. The minimum, and enough to
	// shut out 99% of the noise from the internet.
	IngressAuthBasic EnvAuthType = "BasicAuth"
	// IngressAuthForward delegates to an oauth2-proxy or equivalent. The right
	// choice when people outside the team will use the environment.
	IngressAuthForward EnvAuthType = "ForwardAuth"
)

// AckReidentification is the acknowledgement that anonymizing is not magic.
const AckReidentification = "i-accept-anonymized-data-can-still-be-reidentified"

// EnvExposure describes how the environment is reached.
//
// The rules encode "the same security as production". They are not advice: the
// API server enforces them at apply time.
//
// +kubebuilder:validation:XValidation:rule="!has(self.public) || !self.public || !has(self.auth) || self.auth.type != 'None'",message="a public environment cannot be left unauthenticated: set auth.type to BasicAuth or ForwardAuth"
// +kubebuilder:validation:XValidation:rule="!has(self.public) || !self.public || has(self.host)",message="a public environment needs a host"
type EnvExposure struct {
	// Public declares that the environment is reachable from the internet.
	//
	// Turning it on switches on a set of requirements: mandatory
	// authentication, randomized passwords, closed egress and noindex. It is a
	// security-posture switch, not just a networking one.
	// +optional
	Public *bool `json:"public,omitempty"`

	// +optional
	Host string `json:"host,omitempty"`

	// +kubebuilder:default={}
	// +optional
	Auth EnvAuth `json:"auth,omitempty"`

	// NoIndex adds X-Robots-Tag to the ingress.
	//
	// true by default: a test environment holding realistic-looking data indexed
	// by Google is a leak that requires nobody to attack anything.
	// +kubebuilder:default=true
	// +optional
	NoIndex *bool `json:"noIndex,omitempty"`

	// RateLimitRPS limits requests per second at the ingress. A public Odoo with
	// no limit falls over on its own the moment a rude crawler shows up.
	// +kubebuilder:default=20
	// +optional
	RateLimitRPS *int32 `json:"rateLimitRPS,omitempty"`
}

// EnvAuth is the ingress authentication.
type EnvAuth struct {
	// +kubebuilder:default=BasicAuth
	// +optional
	Type EnvAuthType `json:"type,omitempty"`

	// SecretRef holds "users" (htpasswd format) for BasicAuth, or "url" of the
	// authentication service for ForwardAuth.
	// +optional
	SecretRef string `json:"secretRef,omitempty"`
}

// EnvSecurity holds the environment's internal controls.
type EnvSecurity struct {
	// RandomizeUserPasswords replaces the dump's passwords with a random one,
	// different per environment, stored in a Secret.
	//
	// MANDATORY when the environment is public. The dump OdooSnapshot produces
	// carries the same known password for every user, because a rehearsal
	// without an ingress needs that to be validated by hand. Serving that same
	// dump on a public URL hands over the administrator account.
	// +kubebuilder:default=true
	// +optional
	RandomizeUserPasswords *bool `json:"randomizeUserPasswords,omitempty"`

	// AdminUsers are the logins that keep a usable password, in a Secret the
	// operator generates. Everyone else is left with a random, unusable one.
	//
	// Without this, randomizeUserPasswords leaves an environment nobody can log
	// into. With it, the people who should get in, get in.
	// +kubebuilder:default={"admin"}
	// +optional
	AdminUsers []string `json:"adminUsers,omitempty"`

	// DenyEgress stops the pod from talking to anything but its own database.
	//
	// MANDATORY when public. An exposed Odoo is a target, and what must be
	// prevented is reaching the internal network or the production database from
	// it. With this, compromising it only yields anonymized data.
	// +kubebuilder:default=true
	// +optional
	DenyEgress *bool `json:"denyEgress,omitempty"`

	// StripExternalCredentials deletes from ir_config_parameter the external
	// service keys neutralization does not cover: API tokens, webhook URLs,
	// OAuth secrets.
	//
	// Neutralizing cuts mail, crons, payments and carriers. It does NOT touch
	// the credentials your own modules store there. That is the gap through
	// which a test environment ends up writing into a supplier's ERP.
	// +kubebuilder:default=true
	// +optional
	StripExternalCredentials *bool `json:"stripExternalCredentials,omitempty"`
}

// EnvData is the data source.
type EnvData struct {
	Type EnvDataType `json:"type"`

	// Snapshot is what gets restored when Type is Snapshot. Same generic
	// provider model as OdooRehearsal.
	// +optional
	Snapshot *SnapshotSpec `json:"snapshot,omitempty"`

	// AcknowledgeReidentificationRisk is required to combine snapshot data with
	// public access.
	//
	// Anonymizing is not magic. Deterministic masking deliberately preserves
	// relations, distributions and dates, because without those the environment
	// is useless for testing. That is precisely what allows re-identification:
	// anyone who knows an order total, a specific date or an unusual product can
	// tie the record back to its person.
	//
	// It is a residual risk that is acceptable in many cases, but it is a
	// decision somebody has to make by hand, not a default.
	// +optional
	AcknowledgeReidentificationRisk string `json:"acknowledgeReidentificationRisk,omitempty"`
}

// EnvLifecycle is how long it lives.
type EnvLifecycle struct {
	// +kubebuilder:default=Ephemeral
	// +optional
	Type EnvLifecycleType `json:"type,omitempty"`

	// TTL destroys the environment that long after it was created.
	//
	// Required for Ephemeral: "ephemeral" with no deadline means "permanent and
	// nobody remembers creating it", and a forgotten environment with real data
	// and a public URL is exactly the incident all of this is trying to
	// prevent.
	// +kubebuilder:default="72h"
	// +optional
	TTL *metav1.Duration `json:"ttl,omitempty"`

	// IdleTimeout puts the environment to sleep after that long without
	// requests, when Type is Hibernating.
	// +kubebuilder:default="2h"
	// +optional
	IdleTimeout *metav1.Duration `json:"idleTimeout,omitempty"`
}

// OdooEnvironmentSpec brings up an Odoo.
//
// The three rules that encode "the same security as production" for a public
// environment holding real data:
//
// +kubebuilder:validation:XValidation:rule="!has(self.exposure) || !has(self.exposure.public) || !self.exposure.public || (has(self.security) && (!has(self.security.randomizeUserPasswords) || self.security.randomizeUserPasswords))",message="a public environment requires security.randomizeUserPasswords: the OdooSnapshot dump carries the same known password for every user and would hand the administrator account to anyone"
// +kubebuilder:validation:XValidation:rule="!has(self.exposure) || !has(self.exposure.public) || !self.exposure.public || (has(self.security) && (!has(self.security.denyEgress) || self.security.denyEgress))",message="a public environment requires security.denyEgress: if it is compromised it must not be able to reach the internal network or the production database"
// +kubebuilder:validation:XValidation:rule="!has(self.exposure) || !has(self.exposure.public) || !self.exposure.public || self.data.type != 'Snapshot' || self.data.acknowledgeReidentificationRisk == 'i-accept-anonymized-data-can-still-be-reidentified'",message="combining anonymized data with public access requires data.acknowledgeReidentificationRisk set to its literal value: deterministic masking preserves relations and dates, and that allows re-identification"
// +kubebuilder:validation:XValidation:rule="self.data.type != 'Snapshot' || has(self.data.snapshot)",message="data.type Snapshot requires the data.snapshot field"
// +kubebuilder:validation:XValidation:rule="!has(self.purpose) || self.purpose != 'Production' || self.data.type == 'Live'",message="a Production environment runs the customer's own data: demo data is not production, and an anonymized snapshot called production is a copy people will start trusting. Set data.type to Live, or change the purpose"
// +kubebuilder:validation:XValidation:rule="!has(self.purpose) || self.purpose != 'Production' || !has(self.lifecycle) || self.lifecycle.type == 'Persistent'",message="a Production environment cannot be Ephemeral or Hibernating: the first deletes the customer's Odoo when its time is up and the second switches it off"
// +kubebuilder:validation:XValidation:rule="!has(self.purpose) || (self.purpose != 'Staging' && self.purpose != 'Production') || !has(self.storage) || !has(self.storage.filestore) || self.storage.filestore.mode != 'Ephemeral'",message="a Staging or Production environment cannot keep its filestore in an emptyDir: the database outlives the pod and the files do not, so every attachment breaks on the first restart while ir_attachment still points at them"
// The two combinations that silently lose data, refused at apply time rather than
// discovered when somebody opens an invoice:
//
//   - a Persistent or Hibernating environment with an ephemeral filestore. The
//     database survives the pod and the files do not, so ir_attachment keeps rows
//     pointing at store_fname paths that are gone.
//
//     Note the predicate is lifecycle.type, NOT the absence of a ttl. The first
//     version of this rule tested "no lifecycle.ttl" and never fired, because ttl
//     carries a 72h default and the API server fills it in — so at CEL evaluation
//     time every environment has one. Caught by the guardrail check that expected
//     a rejection and got an accept.
//
//   - more than one web replica without a filestore that is really ReadWriteMany.
//     Each pod would serve its own copy.
//
// Both guard with has() first. self.lifecycle on an object that omitted it is an
// evaluation ERROR, not a missing value, and an erroring rule rejects every
// OdooEnvironment including the valid ones — the trap this project has now hit
// four times.
//
// +kubebuilder:validation:XValidation:rule="!has(self.lifecycle) || self.lifecycle.type == 'Ephemeral' || !has(self.storage) || !has(self.storage.filestore) || self.storage.filestore.mode != 'Ephemeral'",message="a Persistent or Hibernating environment cannot use an Ephemeral filestore (use PersistentVolumeClaim, or Database to keep attachments in Postgres and have no filestore at all): the database outlives the pod and the files do not, so every attachment breaks on the first restart while ir_attachment still points at them"
// +kubebuilder:validation:XValidation:rule="!has(self.workload) || !has(self.workload.web) || self.workload.web.replicas <= 1 || (has(self.storage) && has(self.storage.filestore) && (self.storage.filestore.mode == 'Database' || (self.storage.filestore.mode == 'PersistentVolumeClaim' && self.storage.filestore.accessModeReadWriteMany)))",message="more than one web replica needs a filestore every pod can reach: either PersistentVolumeClaim declared ReadWriteMany, or Database, which has no filestore to share: each pod would otherwise serve its own filestore, so an attachment uploaded through one is a 404 through the other"
// +kubebuilder:validation:XValidation:rule="!has(self.workload) || !has(self.workload.cron) || self.workload.cron.replicas == 0 || (has(self.storage) && has(self.storage.filestore) && (self.storage.filestore.mode == 'Database' || (self.storage.filestore.mode == 'PersistentVolumeClaim' && self.storage.filestore.accessModeReadWriteMany)))",message="a cron tier is a second pod writing the same filestore and needs one both tiers can reach: either PersistentVolumeClaim declared ReadWriteMany, or Database: scheduled jobs that generate reports or attachments would otherwise write them where the web tier cannot read them, and a ReadWriteOnce claim only appears to work while both pods happen to land on the same node"
type OdooEnvironmentSpec struct {
	// Update says when the modules are brought in line with the code.
	// +optional
	Update *UpdateSpec `json:"update,omitempty"`

	// Purpose is what this environment is FOR, and it is the field to fill in
	// first.
	//
	// Everything else in this spec is a mechanism: a data source, a lifecycle, a
	// filestore mode, an exposure. Getting a staging server right means choosing
	// four of them consistently, and the combinations that are wrong are not
	// obviously wrong — a staging server with an Ephemeral filestore loses every
	// attachment on the first restart, and looks fine until somebody opens an
	// invoice.
	//
	// So the purpose expands into those four, in the admission webhook, and only
	// where they were left empty. It is a starting point and not a straitjacket:
	// a consultant who needs a Persistent review environment says so and gets it.
	//
	// What the purpose DOES enforce is the handful of combinations that are not
	// a preference but a mistake — production running demo data, or production
	// that deletes itself after three days.
	// +kubebuilder:validation:Enum=Review;QA;Staging;Production
	// +optional
	Purpose EnvPurpose `json:"purpose,omitempty"`

	// ImageRef names an entry in the customer's image catalogue.
	//
	// The field a person fills in. spec.image is the registry reference it
	// resolves to, written by the mutating webhook — which means an environment
	// records WHICH image it ran even after the catalogue entry is repointed at a
	// new build, and that is the difference between a reproducible environment
	// and one that quietly changed underneath somebody.
	//
	// Naming both is an error rather than a precedence rule: a precedence rule is
	// a thing people have to remember, and getting it wrong here means running a
	// different version of the product than the screen says.
	//
	// That check is in the mutating webhook and NOT a CEL rule, and the reason is
	// worth keeping. A CEL rule saying "not both" runs after admission mutation,
	// by which point the webhook has resolved imageRef into image and produced
	// exactly the state the rule forbids. It rejected every environment that used
	// a catalogue name — a rule firing on a condition the system itself creates.
	// Only the webhook sees what the client actually sent.
	// +kubebuilder:validation:MaxLength=63
	// +optional
	ImageRef string `json:"imageRef,omitempty"`

	// ImageFlavor is how this image is put together. Filled from the customer's
	// catalogue entry when imageRef names one, so it is normally set in exactly
	// one place per image rather than on every environment.
	// +kubebuilder:default=Official
	// +optional
	ImageFlavor ImageFlavor `json:"imageFlavor,omitempty"`

	// +kubebuilder:validation:MinLength=1
	Image string `json:"image"`

	// +optional
	Addons AddonsSpec `json:"addons,omitempty"`

	Data EnvData `json:"data"`

	// Migration runs the module update as a STEP, before the environment is
	// exposed. Empty means no migration: restore and serve.
	//
	// This is the only real difference from OdooRehearsal. There, the result of
	// the `-u` is a gate that decides whether something gets promoted. Here it is
	// simply the errand on the way to a usable environment running a future
	// version — which is exactly what "show the customer their data on Odoo 20"
	// needs. Same machinery, different purpose, and that is why they are two
	// types rather than one with a flag.
	// +optional
	Migration MigrationSpec `json:"migration,omitempty"`

	// ForTenant is the customer this environment is FOR.
	//
	// Setting it turns the environment into a handover, and the handover guardrail
	// applies: the operator refuses to expose it when the data comes from a
	// database that also holds other customers. Leaving it empty means the
	// environment is internal and no handover check runs.
	// +optional
	ForTenant string `json:"forTenant,omitempty"`

	// SourceDatabase is the OdooDatabase the snapshot came from, used by the
	// handover check to find out who else is inside it.
	// +optional
	SourceDatabase string `json:"sourceDatabase,omitempty"`

	// +kubebuilder:default={}
	// +optional
	Lifecycle EnvLifecycle `json:"lifecycle,omitempty"`

	// +kubebuilder:default={}
	// +optional
	Exposure EnvExposure `json:"exposure,omitempty"`

	// Security carries default={} on purpose, and it is not cosmetic: without
	// it, omitting the whole block means its fields' defaults are NOT applied,
	// and an environment would come up without randomized passwords and without
	// closed egress WITHOUT ANYONE ASKING FOR THAT. With the empty default,
	// structural defaulting cascades and the controls stay on.
	// +kubebuilder:default={}
	// +optional
	Security EnvSecurity `json:"security,omitempty"`

	Database DatabaseSpec `json:"database"`

	// RunAsUser is the uid the Odoo containers run as.
	//
	// It defaults to 100, which is what the official Odoo image and OCA's OCB both
	// use. It exists as a field because the operator used to hardcode 65532 — the
	// distroless convention — and Odoo's startup calls getpass.getuser(), which
	// does pwd.getpwuid(os.getuid()) and raises KeyError for a uid that has no
	// passwd entry. Every environment pod failed before Odoo printed a line, with
	// a traceback that says nothing about uids being the problem.
	//
	// Set it if your image uses a different user. Leave it if you do not know:
	// getting it wrong fails loudly and immediately, which is the good case.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:default=100
	// +optional
	RunAsUser *int64 `json:"runAsUser,omitempty"`

	// RunAsGroup and FSGroup default to 101, Odoo's group in the same images.
	// FSGroup is what makes a mounted filestore writable: without it a PVC is owned
	// by root and Odoo cannot write an attachment to it.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:default=101
	// +optional
	RunAsGroup *int64 `json:"runAsGroup,omitempty"`

	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:default=101
	// +optional
	FSGroup *int64 `json:"fsGroup,omitempty"`

	// Storage is where the filestore lives. Absent means ephemeral, which is
	// correct for a throwaway environment and loses data for anything else.
	// +optional
	Storage *StorageSpec `json:"storage,omitempty"`

	// Workload splits the environment's processes. Absent means one deployment
	// serving HTTP and running crons, which is right for a throwaway environment
	// and wrong for a persistent one.
	// +optional
	Workload *WorkloadSplit `json:"workload,omitempty"`

	// Size is the resource class.
	//
	// It carries NO schema default, deliberately, and that is a correction. With
	// `+kubebuilder:default=medium` the API server filled it before any webhook
	// ran, so the customer's own default could never apply: the field was already
	// set by the time anything looked, and per-customer sizing was a field that
	// silently did nothing. An unset size is medium, decided in sizeToResources
	// where the table already lives.
	// +optional
	Size Size `json:"size,omitempty"`
}

// AddonRevision is one repository, as it was actually cloned.
type AddonRevision struct {
	// Name matches spec.addons.repos[].name.
	Name string `json:"name"`

	// Ref is what was asked for: a branch, a tag or a commit.
	// +optional
	Ref string `json:"ref,omitempty"`

	// Revision is the commit it resolved to. With a commit ref the two are the
	// same, and that is the point — a branch drifts and this does not.
	// +optional
	Revision string `json:"revision,omitempty"`

	// ObservedAt is when the clone happened, which is the moment a branch stopped
	// being a moving target for this environment.
	// +optional
	ObservedAt *metav1.Time `json:"observedAt,omitempty"`
}

// ─────────────── Keeping the modules level with the code ───────────────
//
// An environment clones its addons when its pod starts, so a new commit reaches
// the filesystem on the next restart. Nothing then tells Odoo about it: the
// module records in the database still describe the old code, views are not
// reloaded, and a new field has no column. The environment runs new code against
// an old schema, which fails in ways that look like a bug in the change.
//
// odoo.sh solves this by asking people to bump the version in __manifest__.py,
// and updates whatever they bumped. That works and it depends on somebody
// remembering — and on remembering for every module a change touched, which is
// exactly the sort of thing that is right nine times and wrong the tenth.
//
// click-odoo-update does it better: it hashes each addon's file content and
// compares that with the hashes it stored in the database last time, so what
// gets updated is what actually changed. Doblura already requires
// click-odoo-contrib for restores. This uses what is already there.
//
// OnStart defaults by PURPOSE, and the default is the interesting part. A review
// environment exists to look at a change and should absorb it without ceremony.
// A production environment must not update its schema because a node was drained
// at three in the morning and the pod happened to restart.

// UpdateSpec says when modules are updated.
type UpdateSpec struct {
	// OnStart runs click-odoo-update before the server starts, every time the
	// pod starts.
	//
	// Left unset it follows the purpose: on for Review and QA, off for Staging
	// and Production. Set it explicitly and it is honoured — somebody running
	// staging as a rolling rehearsal of production is not wrong.
	//
	// A pointer because false is a real answer that must survive the defaulting:
	// with a plain bool an explicit "no" is indistinguishable from "unset" and
	// would be overwritten by the purpose.
	// +optional
	OnStart *bool `json:"onStart,omitempty"`

	// Modules restricts what is updated. Empty means "whatever changed", which
	// is what click-odoo-update is for and what you want almost always.
	// +optional
	Modules []string `json:"modules,omitempty"`

	// Timeout bounds the update. An update that hangs holds the pod in its init
	// phase, and a review environment that never starts is harder to diagnose
	// than one that starts and says the update failed.
	// +kubebuilder:default="20m"
	// +optional
	Timeout *metav1.Duration `json:"timeout,omitempty"`
}

// UpdatesOnStart reports whether this environment updates its modules as it
// starts, resolving the purpose default when nothing was said.
func (s *OdooEnvironmentSpec) UpdatesOnStart() bool {
	if s.Update != nil && s.Update.OnStart != nil {
		return *s.Update.OnStart
	}
	switch s.Purpose {
	case PurposeReview, PurposeQA:
		return true
	case PurposeStaging, PurposeProduction:
		return false
	}
	// No purpose at all: off. An environment that updates its own schema without
	// anybody asking is a surprise, and surprises belong on the side somebody
	// opted into.
	return false
}

// UpdateTimeout is the bound, with its default.
func (s *OdooEnvironmentSpec) UpdateTimeout() time.Duration {
	if s.Update != nil && s.Update.Timeout != nil && s.Update.Timeout.Duration > 0 {
		return s.Update.Timeout.Duration
	}
	return 20 * time.Minute
}

// EnvPurpose is what an environment is for.
type EnvPurpose string

const (
	// PurposeReview is one copy per pull request, thrown away when it merges.
	PurposeReview EnvPurpose = "Review"
	// PurposeQA is where a change is checked against realistic data before it
	// goes anywhere near a customer.
	PurposeQA EnvPurpose = "QA"
	// PurposeStaging is the long-lived rehearsal of production: same shape, same
	// data, nobody's real invoices.
	PurposeStaging EnvPurpose = "Staging"
	// PurposeProduction is the customer's actual Odoo.
	PurposeProduction EnvPurpose = "Production"
)

// PurposeDefaults is what a purpose means, in the fields it expands into.
//
// Written as a table rather than a switch so the four rows can be read side by
// side. The differences between them are the whole design, and a switch buries
// them in control flow.
type PurposeDefaults struct {
	Lifecycle EnvLifecycleType
	TTL       string
	Data      EnvDataType
	Filestore FilestoreMode
	Size      Size
}

// DefaultsFor returns what a purpose expands to, and whether it is a known one.
func DefaultsFor(p EnvPurpose) (PurposeDefaults, bool) {
	switch p {
	case PurposeReview:
		// Demo data, because a review environment exists to look at a change and
		// most changes do not need real data to look at. Ephemeral filestore is
		// fine: it dies with the environment, and so does everything in it.
		return PurposeDefaults{
			Lifecycle: LifecycleEphemeral, TTL: "72h",
			Data: DataDemo, Filestore: FilestoreEphemeral, Size: SizeSmall,
		}, true
	case PurposeQA:
		// A copy of production, anonymised — checking a change against demo data
		// is how a bug that only appears at scale reaches a customer. A week,
		// because QA runs on a person's calendar and not on a merge.
		return PurposeDefaults{
			Lifecycle: LifecycleEphemeral, TTL: "168h",
			Data: DataSnapshot, Filestore: FilestoreDatabase, Size: SizeMedium,
		}, true
	case PurposeStaging:
		// Persistent, and the filestore has to outlive the pod: this is the one
		// people get wrong, and the failure is silent until an attachment is
		// opened weeks later.
		return PurposeDefaults{
			Lifecycle: LifecyclePersistent,
			Data:      DataSnapshot, Filestore: FilestoreDatabase, Size: SizeMedium,
		}, true
	case PurposeProduction:
		// Live data, and a filestore on a real volume. Large by default because
		// the cost of being one size too small in production is measured in
		// complaints and the cost of being one too big is measured in money.
		return PurposeDefaults{
			Lifecycle: LifecyclePersistent,
			Data:      DataLive, Filestore: FilestorePVC, Size: SizeLarge,
		}, true
	}
	return PurposeDefaults{}, false
}

// EnvPhase summarises the state.
// +kubebuilder:validation:Enum=Pending;Provisioning;Restoring;Hardening;Ready;Hibernated;Expired;Failed
type EnvPhase string

const (
	EnvPending      EnvPhase = "Pending"
	EnvProvisioning EnvPhase = "Provisioning"
	EnvRestoring    EnvPhase = "Restoring"
	// EnvHardening is the phase that applies the controls: randomizing passwords
	// and stripping external credentials. It is a phase of its own rather than a
	// startup detail on purpose: until it finishes, the environment is NOT
	// exposed. Serving an Odoo with the freshly restored dump, even for one
	// second, means serving the known password to the internet.
	EnvHardening  EnvPhase = "Hardening"
	EnvReady      EnvPhase = "Ready"
	EnvHibernated EnvPhase = "Hibernated"
	EnvExpired    EnvPhase = "Expired"
	EnvFailed     EnvPhase = "Failed"
)

// OdooEnvironmentStatus is written by the controller only.
type OdooEnvironmentStatus struct {
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
	// +optional
	Phase EnvPhase `json:"phase,omitempty"`
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// +optional
	URL string `json:"url,omitempty"`

	// CredentialsSecret is the Secret holding the generated passwords for the
	// adminUsers. It is the only way in once passwords are randomized.
	// +optional
	CredentialsSecret string `json:"credentialsSecret,omitempty"`

	// ExpiresAt is when it gets destroyed.
	// +optional
	ExpiresAt *metav1.Time `json:"expiresAt,omitempty"`

	// AddonRevisions is the commit each declared repository was ACTUALLY at.
	//
	// spec.addons.repos[].ref is what was ASKED for, and a branch name is not an
	// answer to "what code is this running". Somebody looking at a broken
	// environment, or at a rehearsal that passed, needs the commit — and the
	// clone container's log, which is where it used to live, disappears with the
	// Job that produced it.
	//
	// +listType=map
	// +listMapKey=name
	// +optional
	AddonRevisions []AddonRevision `json:"addonRevisions,omitempty"`

	// ReadyAt is when this environment first became usable.
	//
	// Not the same as creationTimestamp, and the difference is the whole reason it
	// exists: restoring a 40 GiB snapshot takes minutes to hours, and nobody should
	// be charged for — or credited with — the time before anything was reachable.
	// Set once and never moved, so a hibernate/wake cycle does not reset it.
	// +optional
	ReadyAt *metav1.Time `json:"readyAt,omitempty"`

	// TerminatedAt is when it stopped consuming.
	//
	// Recorded on the object rather than inferred from its disappearance, because a
	// deleted object cannot be asked anything. Whatever accounting exists has to
	// have seen this before the object goes.
	// +optional
	TerminatedAt *metav1.Time `json:"terminatedAt,omitempty"`

	// +optional
	LastRequestTime *metav1.Time `json:"lastRequestTime,omitempty"`

	// +optional
	Message string `json:"message,omitempty"`
}

// OdooEnvironment brings up an Odoo with a declared data source and lifecycle,
// and with the security posture its exposure demands.
//
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=oenv
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Data",type=string,JSONPath=`.spec.data.type`
// +kubebuilder:printcolumn:name="Lifecycle",type=string,JSONPath=`.spec.lifecycle.type`
// +kubebuilder:printcolumn:name="Public",type=boolean,JSONPath=`.spec.exposure.public`
// +kubebuilder:printcolumn:name="URL",type=string,JSONPath=`.status.url`
// +kubebuilder:printcolumn:name="Expires",type=date,JSONPath=`.status.expiresAt`
type OdooEnvironment struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   OdooEnvironmentSpec   `json:"spec,omitempty"`
	Status OdooEnvironmentStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// OdooEnvironmentList is a list of OdooEnvironment.
type OdooEnvironmentList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []OdooEnvironment `json:"items"`
}

func init() {
	SchemeBuilder.Register(&OdooEnvironment{}, &OdooEnvironmentList{})
}

// IsPublic says whether the environment is exposed to the internet.
func (s *OdooEnvironmentSpec) IsPublic() bool {
	return s.Exposure.Public != nil && *s.Exposure.Public
}

// RequiredHardening lists the controls that must be applied before the
// environment is exposed.
//
// Returning a list rather than applying them directly lets the status report
// which ones ran: in an environment holding real data, "what was done" matters
// as much as having done it.
func (s *OdooEnvironmentSpec) RequiredHardening() []string {
	var out []string
	if s.Security.RandomizeUserPasswords == nil || *s.Security.RandomizeUserPasswords {
		out = append(out, "randomize-user-passwords")
	}
	if s.Security.StripExternalCredentials == nil || *s.Security.StripExternalCredentials {
		out = append(out, "strip-external-credentials")
	}
	if s.Security.DenyEgress == nil || *s.Security.DenyEgress {
		out = append(out, "deny-egress")
	}
	if s.IsPublic() {
		out = append(out, "ingress-auth", "no-index", "rate-limit")
	}
	return out
}

// ─────────────── Web, crons and jobs ───────────────
//
// One Odoo deployment serving HTTP *and* running crons is the default because it
// is the simplest thing that works, and it stops working for three separate
// reasons that people usually discover one at a time.
//
//  1. A heavy cron starves the web workers. Odoo's cron threads live in the same
//     process pool as HTTP; a scheduler run that takes four minutes is four minutes
//     of a worker not answering anybody.
//  2. Scaling the web tier multiplies the cron tier. Every replica polls
//     ir_cron on its own timer. Odoo takes a per-job advisory lock so the same job
//     does not execute twice, but the polling, the connections and the wakeups all
//     multiply, and the lock is the only thing between you and a job that was not
//     written to be re-entrant.
//  3. OCA's queue_job runrunner is NOT protected that way. It is a singleton by
//     design, and running two is a supported way to process the same job twice.
//
// So the split is offered, and the guardrails that make it safe are in the API
// rather than in a runbook.
//
// NOTE on neutralized environments, because it changes what this is for: Odoo's
// own neutralize.sql sets `ir_cron.active = false` for every cron except
// autovacuum_job. So in a neutralized copy — the default here — the crons are
// already off in the DATA, and a cron deployment would poll and find nothing. The
// separation matters for persistent staging, for the acknowledged non-neutralized
// case, and for keeping a long cron off the web path. It is not a way to make an
// anonymized environment safe: that job is already done.

// WorkloadSplit says how an environment's processes are divided.
//
// +kubebuilder:validation:XValidation:rule="!has(self.cron) || !has(self.web) || self.web.replicas == 0 || self.cron.replicas <= 1",message="cron.replicas may be at most 1: every replica polls ir_cron independently, and the per-job advisory lock is the only thing preventing a job that was not written to be re-entrant from overlapping with itself"
// +kubebuilder:validation:XValidation:rule="!has(self.queueJob) || self.queueJob.replicas <= 1",message="queueJob.replicas may be at most 1: OCA's queue_job runner is a singleton by design, and a second one is a supported way to process the same job twice"
type WorkloadSplit struct {
	// Web serves HTTP. When a cron section is present this tier runs with
	// max_cron_threads = 0, so crons happen in exactly one place.
	// +optional
	Web *WebTier `json:"web,omitempty"`

	// Cron runs ir.cron and nothing else.
	//
	// Absent means the historical behaviour: one deployment doing both. That is
	// deliberately still the default, because it is right for an ephemeral
	// environment somebody opens for twenty minutes.
	// +optional
	Cron *CronTier `json:"cron,omitempty"`

	// QueueJob runs OCA's queue_job runner.
	//
	// Optional in the strong sense: queue_job is an OCA addon and most
	// installations do not have it. Asking for this tier without the module on the
	// addons path produces a pod that starts and does nothing, so the operator
	// reports that rather than leaving it running.
	// +optional
	QueueJob *QueueJobTier `json:"queueJob,omitempty"`
}

type WebTier struct {
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=32
	// +kubebuilder:default=1
	// +optional
	Replicas int32 `json:"replicas,omitempty"`

	// Workers is Odoo's HTTP worker count.
	//
	// 0 means threaded mode, which is what you want for a small environment: the
	// prefork pool costs memory per worker and buys nothing under one user.
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=64
	// +optional
	Workers *int32 `json:"workers,omitempty"`
}

type CronTier struct {
	// Replicas is 0 or 1. See the guardrail on WorkloadSplit.
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=1
	// +kubebuilder:default=1
	// +optional
	Replicas int32 `json:"replicas,omitempty"`

	// Threads is max_cron_threads on the cron tier.
	//
	// More than one lets independent jobs run in parallel. It does not make a
	// single slow job faster, and it is the setting people raise when the real
	// problem is one job.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=8
	// +kubebuilder:default=2
	// +optional
	Threads int32 `json:"threads,omitempty"`
}

type QueueJobTier struct {
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=1
	// +kubebuilder:default=1
	// +optional
	Replicas int32 `json:"replicas,omitempty"`

	// Channels is queue_job's channel configuration, e.g. "root:2,export:1".
	// Passed through unread: it is queue_job's syntax, and validating somebody
	// else's grammar here would only ever be wrong in a new version.
	// +kubebuilder:validation:MaxLength=512
	// +optional
	Channels string `json:"channels,omitempty"`
}

// SplitsCrons reports whether crons are handled by their own tier.
//
// When true the web tier must run with max_cron_threads = 0. Getting that wrong is
// the whole failure this feature exists to prevent: the crons would run in BOTH
// places, which looks like it is working right up until a job that assumed it was
// alone runs twice.
func (w *WorkloadSplit) SplitsCrons() bool {
	return w != nil && w.Cron != nil && w.Cron.Replicas > 0
}

// CronThreadsForWeb is what max_cron_threads must be on the HTTP tier.
func (w *WorkloadSplit) CronThreadsForWeb() int32 {
	if w.SplitsCrons() {
		return 0
	}
	return 1
}

// CronThreads is what max_cron_threads must be on the cron tier.
//
// Only ever read when SplitsCrons() is true, so the zero case is a defence
// against a future caller rather than a reachable state today: a cron tier
// configured with no cron threads is a pod that runs Odoo and schedules nothing,
// which is the same silent failure as having no tier at all.
func (w *WorkloadSplit) CronThreads() int32 {
	if !w.SplitsCrons() || w.Cron.Threads <= 0 {
		return 1
	}
	return w.Cron.Threads
}

// ─────────────── The filestore ───────────────
//
// Odoo's filestore is mutable state that lives OUTSIDE the database, and it is the
// clearest place where Odoo is not a normal stateless application. Every
// attachment, every generated report, every uploaded image is a file on disk, and
// ir_attachment.store_fname is a pointer to it. The database and the filestore are
// one artifact in two places.
//
// This was an emptyDir. Three things followed, and the third is embarrassing:
//
//  1. A persistent environment lost every attachment on any pod restart, while the
//     database kept the ir_attachment rows pointing at files that no longer
//     existed. Not an error at restart — an error later, when somebody opens an
//     invoice.
//  2. More than one replica meant more than one filestore. Upload on pod A, 404 on
//     pod B, with nothing in any log to say why.
//  3. The Deployment used the Recreate strategy with the comment "the filestore is
//     RWO" — justifying a real constraint with an architecture that was not there.
//     An emptyDir is not RWO, and Recreate saves nothing when the data dies with
//     the pod either way.
//
// So the mode is now explicit, and the API refuses the combination that silently
// loses data.

// FilestoreMode is where Odoo's filestore lives.
// +kubebuilder:validation:Enum=Ephemeral;PersistentVolumeClaim;Database
type FilestoreMode string

const (
	// FilestoreEphemeral is an emptyDir: it dies with the pod.
	//
	// Correct, and the default, for an environment somebody opens for eight hours
	// to reproduce a ticket. Wrong for anything that outlives a pod.
	FilestoreEphemeral FilestoreMode = "Ephemeral"

	// FilestorePVC is a PersistentVolumeClaim: real read/write while Odoo serves,
	// surviving restarts.
	//
	// This is also the answer for "somewhere external", and deliberately the only
	// one. Whatever backs the claim — NFS, CephFS, JuiceFS, an S3 CSI driver,
	// Azure Files — is a StorageClass concern, and the operator neither knows nor
	// needs to. Growing S3 credentials here would be re-implementing what the
	// platform already expresses, and it would be five providers of code that
	// still falls short on the sixth. Same reasoning as the snapshot providers.
	FilestorePVC FilestoreMode = "PersistentVolumeClaim"

	// FilestoreDatabase puts attachments in Postgres and has no filestore at all.
	//
	// This is Odoo core, not an addon: ir_attachment reads the system parameter
	// `ir_attachment.location`, and 'db' stores bytes in ir_attachment.db_datas
	// instead of on disk. Core supports exactly two values, 'file' and 'db', and
	// ships force_storage() to move existing attachments between them.
	//
	// It solves a problem this project warns about everywhere else: "restoring a
	// database without its filestore leaves orphaned attachments, and that breaks
	// migrations in confusing ways." With 'db' there is no separate filestore to
	// lose — the dump IS the whole artifact, and a rehearsal cannot be quietly
	// wrong because somebody copied one half.
	//
	// The cost, which is real and is why this is not the default: Postgres becomes
	// a blob store. The database grows by the whole attachment volume, every dump
	// and restore carries it, and on a database where attachments are most of the
	// bytes that turns a fast snapshot into a slow one. Sensible for ephemeral and
	// rehearsal environments; think before choosing it for staging, and do not
	// assume it for production.
	FilestoreDatabase FilestoreMode = "Database"
)

// FilestoreSpec says where the filestore lives and admits what cannot be checked.
//
// +kubebuilder:validation:XValidation:rule="self.mode != 'PersistentVolumeClaim' || has(self.claimName) || has(self.size)",message="a PersistentVolumeClaim filestore needs either claimName (a PVC you manage) or size (one to create)"
// +kubebuilder:validation:XValidation:rule="self.mode != 'Ephemeral' || !has(self.claimName)",message="claimName is meaningless with mode Ephemeral; the filestore would still be an emptyDir and the claim would go unused, which is worse than an error"
// +kubebuilder:validation:XValidation:rule="self.mode != 'Database' || (!has(self.claimName) && !has(self.size))",message="mode Database keeps attachments in Postgres, so there is no filestore for a claim or a size to describe"
type FilestoreSpec struct {
	// +kubebuilder:default=Ephemeral
	// +optional
	Mode FilestoreMode `json:"mode,omitempty"`

	// ClaimName is an existing PVC. Use this when something else manages the
	// volume's lifecycle — which is usually right for staging, because the
	// filestore should outlive the OdooEnvironment object.
	// +kubebuilder:validation:MaxLength=253
	// +optional
	ClaimName string `json:"claimName,omitempty"`

	// Size creates a PVC owned by this environment. It is deleted with it, so a
	// staging built this way loses its attachments when the object is deleted.
	// +kubebuilder:validation:Pattern=`^[0-9]+(Mi|Gi|Ti)$`
	// +optional
	Size string `json:"size,omitempty"`

	// StorageClass for a created PVC.
	// +kubebuilder:validation:MaxLength=253
	// +optional
	StorageClass string `json:"storageClass,omitempty"`

	// AccessModeReadWriteMany declares that the volume really is RWX.
	//
	// Declared rather than detected, and the distinction is the honest part: a
	// StorageClass's supported access modes are not reliably knowable from here,
	// and a PVC that binds RWO while claiming RWX fails at scheduling rather than
	// at write time. So the operator requires the claim and refuses more than one
	// replica without it, instead of pretending it verified something.
	//
	// Get it wrong in the optimistic direction and the second pod either never
	// schedules or serves a different filestore from the first.
	// +optional
	AccessModeReadWriteMany bool `json:"accessModeReadWriteMany,omitempty"`
}

// FilestoreIsEphemeral reports whether the filestore dies with the pod.
// FilestoreInDatabase reports whether attachments live in Postgres.
func (s *OdooEnvironmentSpec) FilestoreInDatabase() bool {
	return s.Storage != nil && s.Storage.Filestore != nil &&
		s.Storage.Filestore.Mode == FilestoreDatabase
}

func (s *OdooEnvironmentSpec) FilestoreIsEphemeral() bool {
	return s.Storage == nil || s.Storage.Filestore == nil ||
		s.Storage.Filestore.Mode == "" || s.Storage.Filestore.Mode == FilestoreEphemeral
}

// StorageSpec groups what an environment keeps.
type StorageSpec struct {
	// +optional
	Filestore *FilestoreSpec `json:"filestore,omitempty"`
}

// PodUser returns the uid, gid and fsGroup for this environment's Odoo containers.
//
// Defaults are Odoo's, not Kubernetes'. The distroless 65532 that used to be
// hardcoded here belongs to images built for it; the images this operator actually
// runs use 100, and Odoo is the one that notices, because it resolves its own uid
// through /etc/passwd at startup.
func (s *OdooEnvironmentSpec) PodUser() (uid, gid, fsGroup int64) {
	uid, gid, fsGroup = 100, 101, 101
	if s.RunAsUser != nil {
		uid = *s.RunAsUser
	}
	if s.RunAsGroup != nil {
		gid = *s.RunAsGroup
	}
	if s.FSGroup != nil {
		fsGroup = *s.FSGroup
	}
	return uid, gid, fsGroup
}
