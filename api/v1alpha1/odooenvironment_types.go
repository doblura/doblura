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

// WAFMode is who inspects requests, if anybody.
// +kubebuilder:validation:Enum=None;InCluster;Provider
type WAFMode string

const (
	// WAFNone is nobody. The default, and stated rather than implied.
	WAFNone WAFMode = "None"
	// WAFInCluster is Coraza, running in the ingress proxy.
	WAFInCluster WAFMode = "InCluster"
	// WAFProvider is somebody else's — a cloud load balancer's WAF, driven by
	// annotations doblura copies onto the Ingress and does not interpret.
	WAFProvider WAFMode = "Provider"
)

// WAFEnforcement is what happens when a rule matches.
// +kubebuilder:validation:Enum=Block;Detect
type WAFEnforcement string

const (
	// WAFBlock refuses the request.
	WAFBlock WAFEnforcement = "Block"
	// WAFDetect logs and lets it through. For finding out what a rule would have
	// done to a real customer before it does it.
	WAFDetect WAFEnforcement = "Detect"
)

// EnvWAF is request inspection at the edge.
//
// Two modes, and the honest description of each matters more than the field.
//
// **Provider** copies annotations onto the Ingress. A cloud load balancer's WAF is
// configured by its own controller in its own vocabulary; doblura passes the
// annotations through and interprets none of them. It cannot tell you whether the
// thing is on, and says so rather than reporting a state it did not check.
//
// **InCluster** is Coraza in the ingress proxy, and comes with a limitation worth
// stating before anybody relies on it: the Traefik build of Coraza is WebAssembly
// and has no filesystem, so it CANNOT load the OWASP Core Rule Set. Measured, not
// assumed — `Include @owasp_crs/*.conf` fails with "file does not exist", and the
// WAF then fails OPEN: every request passes uninspected with the reason only in
// the proxy's log.
//
// So what doblura writes here is deliberately not a general web application
// firewall. It is a short list of rules about Odoo's own front door — the database
// manager, and optionally the RPC endpoints — which are rules anybody can read and
// judge. Writing a homemade replacement for the CRS would be the classic mistake:
// worse coverage than the real thing, and a name that implies otherwise. For CRS,
// put a load balancer that has it in front and use Provider mode.
//
// +kubebuilder:validation:XValidation:rule="self.mode != 'Provider' || (has(self.annotations) && size(self.annotations) > 0)",message="Provider mode passes annotations to the load balancer's own controller, so it needs at least one: with none, doblura would report a WAF that nothing was asked to switch on"
type EnvWAF struct {
	// +kubebuilder:default=None
	// +optional
	Mode WAFMode `json:"mode,omitempty"`

	// Enforcement is Block or Detect. Detect first is the right order on an
	// environment somebody depends on: a rule that turns out to match a
	// legitimate request blocks a customer's work, and the way to find that out
	// is to watch it for a week rather than to reason about it.
	// +kubebuilder:default=Block
	// +optional
	Enforcement WAFEnforcement `json:"enforcement,omitempty"`

	// BlockDatabaseManager refuses /web/database/*, which creates, drops,
	// backs up and restores databases.
	//
	// On by default. Odoo's own list_db = False already hides it and doblura sets
	// that, so this is the second lock rather than the first — and it is the lock
	// that still holds if somebody edits the configuration, restores a dump with
	// its own odoo.conf, or runs an image that ships different defaults.
	// +kubebuilder:default=true
	// +optional
	BlockDatabaseManager *bool `json:"blockDatabaseManager,omitempty"`

	// BlockRPC refuses /jsonrpc and /xmlrpc/*.
	//
	// OFF by default, and it must be: the external API is how integrations,
	// e-commerce fronts and the customer's own scripts talk to Odoo, and turning
	// it off at the edge breaks them silently and from a place nobody thinks to
	// look. On for an environment nothing integrates with, which is most staging.
	// +optional
	BlockRPC *bool `json:"blockRPC,omitempty"`

	// ExtraDirectives are appended verbatim, for rules doblura does not write.
	//
	// The escape hatch, and it is a sharp one: a directive Coraza cannot parse
	// stops the whole WAF from starting, and it then lets everything through with
	// the reason only in the proxy's log. Change these on a Detect environment
	// first.
	// +optional
	ExtraDirectives []string `json:"extraDirectives,omitempty"`

	// Annotations go on the Ingress for Provider mode, verbatim.
	// +optional
	Annotations map[string]string `json:"annotations,omitempty"`
}

// Inspects reports whether anything is inspecting requests.
func (w *EnvWAF) Inspects() bool {
	return w != nil && w.Mode != "" && w.Mode != WAFNone
}

// BlocksDatabaseManager defaults to true.
func (w *EnvWAF) BlocksDatabaseManager() bool {
	return w != nil && (w.BlockDatabaseManager == nil || *w.BlockDatabaseManager)
}

// BlocksRPC defaults to false.
func (w *EnvWAF) BlocksRPC() bool {
	return w != nil && w.BlockRPC != nil && *w.BlockRPC
}

// TLSState is whose certificate answers on the address.
// +kubebuilder:validation:Enum=Issued;DefaultCertificate
type TLSState string

const (
	// TLSIssued means a certificate exists for this host, either obtained by
	// cert-manager or loaded by hand.
	TLSIssued TLSState = "Issued"
	// TLSDefaultCertificate means nobody is issuing one and the ingress
	// controller is answering with its own. Browsers will warn.
	TLSDefaultCertificate TLSState = "DefaultCertificate"
)

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

	// Host is the address it answers on.
	//
	// Left empty, it is generated from the customer's domain — see
	// OdooTenantSpec.Domain — except for Production, which is never generated.
	// Written into the spec at admission rather than resolved at reconcile time,
	// so it is the same address for the life of the environment: a hostname
	// recomputed on every reconcile is a hostname that changes under a running
	// certificate.
	// +kubebuilder:validation:MaxLength=253
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

	// AllowFrom restricts who may reach it at all, as CIDR blocks.
	//
	// Before authentication rather than instead of it. A password in front of a
	// staging environment stops somebody who finds the address; a network
	// restriction stops them reaching the application at all, which also means
	// they cannot reach whatever is unpatched in it this week.
	//
	// Empty means anywhere, which is the honest default: doblura does not know
	// the customer's networks, and a made-up range would lock out the people who
	// need it while reading as a protection.
	// +optional
	AllowFrom []string `json:"allowFrom,omitempty"`

	// WAF inspects requests before Odoo sees them.
	// +optional
	WAF *EnvWAF `json:"waf,omitempty"`

	// HSTS tells browsers to refuse http for this host for a year.
	//
	// Defaults to on for a public environment and off otherwise. It is worth
	// knowing that this is remembered by the browser and cannot be taken back
	// within the year: it is set for the host only, never for subdomains and
	// never with preload, because environments live on generated names under a
	// customer's domain and a claim over the whole domain from one of them would
	// reach names doblura did not create and cannot fix.
	// +optional
	HSTS *bool `json:"hsts,omitempty"`
}

// NoIndexes reports whether the noindex header is sent. Default on.
func (e *EnvExposure) NoIndexes() bool {
	return e.NoIndex == nil || *e.NoIndex
}

// SendsHSTS reports whether the strict-transport header is sent.
//
// On by default for a public environment: it is served over https and a browser
// that remembers to skip the http round trip is one that cannot be stripped on a
// hostile network. Off by default otherwise, because an environment reached
// internally may legitimately be plain http, and a year-long browser memory is
// not something to switch on by accident.
func (e *EnvExposure) SendsHSTS() bool {
	if e.HSTS != nil {
		return *e.HSTS
	}
	return e.Public != nil && *e.Public
}

// EnvAuth is the ingress authentication.
type EnvAuth struct {
	// +kubebuilder:default=BasicAuth
	// +optional
	Type EnvAuthType `json:"type,omitempty"`

	// SecretRef holds "users" (htpasswd format) for BasicAuth, or "url" of the
	// authentication service for ForwardAuth.
	//
	// Optional for BasicAuth: left empty, doblura generates one. It was optional
	// before too, and a public environment with BasicAuth and no secret produced
	// a middleware pointing at a Secret that did not exist — which fails open or
	// closed depending on the version, and neither belongs in front of a
	// customer's data.
	// +optional
	SecretRef string `json:"secretRef,omitempty"`

	// ForwardURL is the authentication service, for ForwardAuth.
	// +optional
	ForwardURL string `json:"forwardURL,omitempty"`
}

// URL is where ForwardAuth sends the request.
func (a *EnvAuth) URL() string { return a.ForwardURL }

// EnvSecurity holds the environment's internal controls.
type EnvSecurity struct {
	// RandomizeUserPasswords replaces the dump's passwords with a random one,
	// different per environment, stored in a Secret.
	//
	// MANDATORY when the environment is public. The dump OdooSnapshot produces
	// carries the same known password for every user, because a rehearsal
	// without an ingress needs that to be validated by hand. Serving that same
	// dump on a public URL hands over the administrator account.
	//
	// NOT ON PRODUCTION, where it is refused outright: that is the customer's real
	// system and this locks every one of their users out.
	//
	// No schema default, deliberately. It defaults to on for every purpose but
	// Production, and a `+kubebuilder:default=true` is applied by the API server
	// before anything else runs — so nothing downstream, webhook or CEL, can tell
	// a value somebody asked for from a value the schema filled in. With the
	// default in place the rule below refused every Production environment ever
	// created, including ones whose author had never heard of this field.
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
	//
	// NOT ON PRODUCTION, where it is refused outright: those are the real tokens
	// the customer's integrations run on. No schema default, for the reason given
	// on RandomizeUserPasswords.
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
// +kubebuilder:validation:XValidation:rule="!has(self.mail) || (has(self.purpose) && self.purpose == 'Production') || (has(self.mail.unsafeAcknowledgement) && self.mail.unsafeAcknowledgement == 'i-accept-this-environment-can-send-real-email-to-real-people')",message="configuring outgoing mail on anything but a Production environment requires mail.unsafeAcknowledgement set to its literal value: a working SMTP server on a copy of production sends real invoices and real payment reminders to real people, from a machine nobody is watching, and there is no undo"
// +kubebuilder:validation:XValidation:rule="!has(self.mail) || self.data.type != 'Demo'",message="an environment running demo data has no real addresses to write to, and every message it sends goes to an invented one. If you are testing the mail server itself, use Snapshot data with the acknowledgement, or Live"
// +kubebuilder:validation:XValidation:rule="!has(self.purpose) || self.purpose != 'Production' || !has(self.security) || !has(self.security.randomizeUserPasswords) || !self.security.randomizeUserPasswords",message="security.randomizeUserPasswords cannot be asked for on a Production environment: that is the customer's real system, and this locks every one of their users out. It is what makes a COPY safe, and doblura does not run it here at all — accepting the field would be promising something that will not happen"
// +kubebuilder:validation:XValidation:rule="!has(self.purpose) || self.purpose != 'Production' || !has(self.security) || !has(self.security.stripExternalCredentials) || !self.security.stripExternalCredentials",message="security.stripExternalCredentials cannot be asked for on a Production environment: those are the real API tokens and webhook URLs the customer's integrations run on, not a copy's inherited ones"
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
	// Mail configures outgoing email. Absent means none, which is the right
	// answer for every environment that is not production.
	// +optional
	Mail *MailSpec `json:"mail,omitempty"`

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

// ─────────────── Outgoing email ───────────────
//
// This is the setting where a mistake reaches the customer's customers.
//
// Everything else in this operator can be got wrong and stay inside the cluster.
// A working SMTP server on a copy of production sends real invoices, real payment
// reminders and real delivery notices to real people, from a machine nobody is
// watching, and there is no undo. It is the reason `neutralize` defaults to true
// everywhere and why the harden phase switches every ir_mail_server off.
//
// So mail is not just another field. Configuring it on anything other than a
// Production environment requires saying out loud that you mean it, and the
// literal says what will happen rather than that you have read a warning.
//
// What this does NOT do is run a mail server, relay anything, or hold a mailbox.
// It writes an ir_mail_server row pointing at a server you already have. odoo.sh
// offers "unlimited email gateways, auto configured" because they run the mail
// infrastructure; doblura runs in your cluster and does not, and pretending
// otherwise would be the difference between a tool and a service.

// MailEncryption is how the SMTP session is protected.
// +kubebuilder:validation:Enum=None;StartTLS;SSL
type MailEncryption string

const (
	// MailNone is plaintext SMTP. Allowed, because an in-cluster relay on port 25
	// is a real arrangement, and refusing it would push people to work around
	// this field entirely.
	MailNone     MailEncryption = "None"
	MailStartTLS MailEncryption = "StartTLS"
	MailSSL      MailEncryption = "SSL"
)

// MailAck is the literal that authorises real mail from a non-production
// environment.
const MailAck = "i-accept-this-environment-can-send-real-email-to-real-people"

// MailSpec is the outgoing mail server Odoo will use.
//
// +kubebuilder:validation:XValidation:rule="!has(self.smtpUser) || has(self.passwordSecret)",message="an SMTP user needs a password: set passwordSecret to a Secret holding the key 'password'"
type MailSpec struct {
	// Host is the SMTP server.
	// +kubebuilder:validation:MinLength=1
	Host string `json:"host"`

	// +kubebuilder:default=587
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=65535
	// +optional
	Port int32 `json:"port,omitempty"`

	// +kubebuilder:default=StartTLS
	// +optional
	Encryption MailEncryption `json:"encryption,omitempty"`

	// +optional
	SMTPUser string `json:"smtpUser,omitempty"`

	// PasswordSecret holds the key "password".
	// +optional
	PasswordSecret string `json:"passwordSecret,omitempty"`

	// FromFilter restricts which sender addresses use this server, which is
	// Odoo's own mechanism for having more than one.
	// +optional
	FromFilter string `json:"fromFilter,omitempty"`

	// UnsafeAcknowledgement is required unless the purpose is Production.
	//
	// The literal names the consequence and not the reading of a warning,
	// because the failure this guards against is somebody copying a production
	// manifest into staging and only the mail block being wrong.
	// +optional
	UnsafeAcknowledgement string `json:"unsafeAcknowledgement,omitempty"`
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

	// Rollback takes a copy of the database before updating and puts it back if
	// the update fails.
	//
	// This is what makes updating production something other than frightening. A
	// module update that fails halfway leaves a database that is neither the old
	// schema nor the new one: some tables altered, some not, and the registry
	// describing a state that does not exist. There is no forward fix for that
	// and the only real recovery is a restore — so the copy is taken while
	// somebody is still watching, rather than found to be missing afterwards.
	//
	// odoo.sh does the same thing and calls it reverting to the previous working
	// state. The mechanism here is click-odoo-copydb, which uses Postgres's own
	// CREATE DATABASE ... TEMPLATE and copies the filestore beside it.
	//
	// Two costs, both real and both worth saying rather than discovering:
	//
	//   - It needs as much free disk as the database occupies, for as long as the
	//     update runs. A 40 GiB database needs 40 GiB spare.
	//   - CREATE DATABASE ... TEMPLATE refuses while anything else is connected.
	//     The update runs in an init container with the server not yet started
	//     and the Deployment on the Recreate strategy, so nothing is — but an
	//     external tool holding a session will make the copy fail, and the
	//     message says so.
	//
	// Left unset it follows the purpose: off for Review, on for QA, Staging and
	// Production. A review environment holds demo data and the recovery is to
	// delete it; paying for a copy of that would be paying for nothing.
	// +optional
	Rollback *bool `json:"rollback,omitempty"`
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

// RollsBack reports whether a failed update is undone, resolving the purpose
// default when nothing was said.
func (s *OdooEnvironmentSpec) RollsBack() bool {
	if s.Update != nil && s.Update.Rollback != nil {
		return *s.Update.Rollback
	}
	switch s.Purpose {
	case PurposeQA, PurposeStaging, PurposeProduction:
		return true
	}
	// Review, and anything with no purpose at all: no. Demo data, and the
	// recovery is to delete the environment.
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

	// TLS says whose certificate answers on this address.
	//
	// It exists because the alternative is a status that reads https:// while the
	// ingress controller serves its own self-signed default — every address
	// works, every browser warns, and people learn to click through certificate
	// warnings, which is the habit that makes the warning worthless.
	// +optional
	TLS TLSState `json:"tls,omitempty"`

	// WAF is what was configured to inspect requests, and it is worth being
	// precise about what this does NOT say.
	//
	// It reports the mode doblura wrote, not that inspection is happening. Coraza
	// fails OPEN: a directive it cannot parse stops the WAF from starting and
	// every request then passes uninspected, with the reason only in the ingress
	// proxy's log. In Provider mode doblura only copied annotations and never saw
	// the thing they configure. Neither is something this operator can check, and
	// a status that implied otherwise would be worse than no status.
	// +optional
	WAF WAFMode `json:"waf,omitempty"`

	// PausedBy is the OdooRestore that has this environment stopped, or empty.
	//
	// A restore has to stop the workload: you cannot replace a database under a
	// running Odoo. It used to do that by rewriting spec.lifecycle.type to
	// Hibernating — using a field the CUSTOMER declared as an internal control —
	// and the day a rule said a Production environment may not be Hibernating,
	// restoring into production became impossible. Measured: "a Production
	// environment cannot be Ephemeral or Hibernating", on the one restore that
	// most needs to work.
	//
	// In the status because that is doblura's to write, and because it says WHY
	// something is stopped. "Hibernated" on a production environment is a sentence
	// somebody would page about.
	// +optional
	PausedBy string `json:"pausedBy,omitempty"`

	// ExpiresAt is when it gets destroyed.
	// +optional
	// Printed as a string rather than a date column: kubectl renders a date
	// column as the time SINCE the value, and an expiry is in the future, which
	// it prints as "<invalid>". The column that says when this gets deleted read
	// as broken for the whole of the environment's life.
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
// +kubebuilder:printcolumn:name="Expires",type=string,JSONPath=`.status.expiresAt`
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
