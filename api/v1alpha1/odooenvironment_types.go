// SPDX-License-Identifier: Apache-2.0
// Copyright (C) 2026 Antoni Romera

package v1alpha1

import (
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
type OdooEnvironmentSpec struct {
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

	// +kubebuilder:default=medium
	// +optional
	Size Size `json:"size,omitempty"`
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
