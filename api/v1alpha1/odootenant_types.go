// SPDX-License-Identifier: Apache-2.0
// Copyright (C) 2026 Antoni Romera

package v1alpha1

import (
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ─────────────── The customer ───────────────
//
// OdooTenant is the customer, and almost nothing else. Its databases are not
// listed here: they declare which tenant they belong to, because a database can
// hold several customers and the ownership has to be stated in one place. See
// the comment at the top of odoodatabase_types.go.
//
// It is deliberately thin, and it earns its place anyway:
//
//   - It is the row every persona shares in the interface. Support looks at a
//     customer and wants a scratch database; QA looks at the same customer and
//     wants the staging approval. One list, different actions.
//   - It is what OdooRelease batches over when rolling a product version out
//     customer by customer.
//   - It is the subject of the handover question: "can this copy go to them?"

// OdooTenantSpec describes a customer.
//
// +kubebuilder:validation:XValidation:rule="!has(self.images) || self.images.filter(i, has(i.default) && i.default).size() <= 1",message="only one image may be the default: with two, which one a new environment gets depends on list order, and list order is not something anybody edits on purpose"
// +kubebuilder:validation:XValidation:rule="!has(self.majorUpgrade) || (has(self.images) && self.images.exists(i, i.name == self.majorUpgrade.toImage))",message="majorUpgrade.toImage must name an entry in this customer's image catalogue"
type OdooTenantSpec struct {
	// DisplayName is what humans call them. The object name is a DNS label and
	// makes a poor thing to show people.
	// +kubebuilder:validation:MinLength=1
	DisplayName string `json:"displayName"`

	// ProductRelease is the OdooRelease of your own product this customer is on,
	// when there is a product. Empty means a pure bespoke project.
	//
	// Declaring the release rather than copying the product's code is the whole
	// difference between being able to ship a new product version to thirty
	// customers and maintaining thirty forks.
	// +optional
	ProductRelease string `json:"productRelease,omitempty"`

	// OdooVersion the customer runs, for example "19.0". Recorded here because
	// the version matrix — who is on what — is the question consultancy work
	// asks most often, and nowhere else answers it.
	// +optional
	// +kubebuilder:validation:Pattern=`^[0-9]+\.[0-9]+$`
	OdooVersion string `json:"odooVersion,omitempty"`

	// Default marks this as the customer record used when an environment does not
	// name one.
	//
	// It exists for the company that runs its own Odoo rather than forty of
	// somebody else's. Doblura's model is the integrator's — a customer record, a
	// namespace per customer, quotas per customer — and without this that company
	// pays for a model built for a problem it does not have: no image catalogue,
	// no generated address, no certificate issuer and no defaults, unless it
	// writes forTenant on every environment it ever creates.
	//
	// One record, marked once, and the word is never typed again. That is the
	// whole tax, and it is meant to stay that size: anything more and "doblura
	// serves both" quietly becomes "doblura serves integrators, and is worse for
	// everybody else".
	//
	// At most one per namespace — see the webhook. Two would make which defaults
	// apply depend on iteration order, which is the kind of thing that is right
	// for months and then is not.
	//
	// Note it makes environments STRICTER, not looser: an environment with a
	// customer attached is a handover, and the handover guardrail applies to it.
	// +optional
	Default *bool `json:"default,omitempty"`

	// Domain is where this customer's environments are published, for example
	// "acme.doblura.example" or a domain of the customer's own.
	//
	// Environments other than production get a hostname under it, generated once
	// and kept: <environment>-<six characters>.<domain>. The random part is the
	// point. A staging or a support environment holds the customer's real data
	// behind whatever the ingress asks for, and "staging.acme.example" is found by
	// anybody who tries the obvious name — which is the whole attack. Six random
	// characters make the address something that has to be given to you.
	//
	// Production is never generated. It is the customer's real address, guessing
	// it is worse than asking, and an environment answering on a name nobody
	// chose is not a production environment anybody should trust.
	//
	// Customers usually share one domain and do not have to: set a different one
	// per customer and nothing else changes.
	// +kubebuilder:validation:MaxLength=253
	// +kubebuilder:validation:Pattern=`^([a-z0-9]([-a-z0-9]*[a-z0-9])?\.)+[a-z]{2,}$`
	// +optional
	Domain string `json:"domain,omitempty"`

	// CertIssuer is the cert-manager Issuer that gets certificates for this
	// customer's addresses, as "<kind>/<name>" — for example
	// "ClusterIssuer/letsencrypt" or "Issuer/internal-ca".
	//
	// Left empty, doblura does not claim a certificate it cannot obtain. That
	// distinction is the reason this field exists: the Ingress used to declare a
	// TLS secret unconditionally, nothing ever created it, and the ingress
	// controller quietly served its own default certificate instead. Every
	// address worked, every browser warned, and the environment's status said
	// https:// with no hint that the padlock was broken.
	// +kubebuilder:validation:Pattern=`^(ClusterIssuer|Issuer)/[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	// +optional
	CertIssuer string `json:"certIssuer,omitempty"`

	// MaxEphemeralEnvironments caps how many throwaway environments may exist
	// for this customer at once. The validating admission webhook refuses the
	// create that would exceed it.
	//
	// A quota, not a limit for its own sake: if support opens an environment per
	// ticket the cluster dies on Friday. It lives on the tenant rather than
	// globally so a demanding customer cannot starve the others.
	//
	// Zero is a real answer, and it means zero: a customer whose data nobody may
	// copy any more. Which is why this is a pointer — with a plain int32 and
	// `omitempty`, a Go client asking for zero would drop the field on the wire
	// and get the default of 3 instead, silently granting what it tried to deny.
	// +kubebuilder:default=3
	// +kubebuilder:validation:Minimum=0
	// +optional
	MaxEphemeralEnvironments *int32 `json:"maxEphemeralEnvironments,omitempty"`

	// Images is the catalogue of images this customer may run.
	//
	// A catalogue rather than a free-text field, because nobody should have to
	// remember that this customer runs `ghcr.io/example/hms:18` while the one
	// below them runs something else. Support picks a name from a list; the
	// registry reference is written once, by whoever builds the images.
	//
	// A listType=map keyed on name, so the API server enforces uniqueness itself
	// rather than a CEL rule doing it in quadratic time.
	// +listType=map
	// +listMapKey=name
	// +kubebuilder:validation:MaxItems=32
	// +optional
	Images []ImageCatalogueEntry `json:"images,omitempty"`

	// MajorUpgrade authorises a change of major version, and nothing else.
	//
	// It exists because promoting a customer from 18 to 19 must not be the same
	// gesture as promoting them from one 18 build to the next. The first is a
	// rollout: the image changes, the schema does not, and rolling back is
	// redeploying the old tag. The second rewrites the database, and there is no
	// rollback — there is only a restore from before it started.
	//
	// A dropdown cannot tell those apart, so the API does: changing which
	// catalogue entry is default WITHIN a major is an ordinary edit, and changing
	// it ACROSS majors is refused unless this field names a rehearsal that
	// actually succeeded against exactly that image.
	// +optional
	MajorUpgrade *MajorUpgrade `json:"majorUpgrade,omitempty"`

	// EnvironmentDefaults is how this customer's environments are built.
	//
	// It exists because of who creates environments. Support opens a throwaway
	// copy to reproduce a bug; QA opens one to check a fix. Neither of them knows
	// the Postgres host, the credential Secret or the filestore mode for this
	// customer, and neither of them should have to — that is the platform team's
	// decision, made once, per customer.
	//
	// Without this the OdooEnvironment CRD is a form that cannot be filled in by
	// the people it is for: spec.image, spec.database.host, spec.database.user
	// and spec.database.passwordSecret are all required, and all four are
	// infrastructure. The mutating webhook fills them from here, so `kubectl
	// apply` with a four-line environment works exactly as the console does —
	// putting this in the console instead would have made the interface a
	// privileged path, which is the thing the impersonation design refuses.
	//
	// Anything set explicitly on the environment wins. These are defaults, not
	// policy: a consultant pointing a rehearsal at a different server is a
	// legitimate thing to do, and the quota is what bounds the damage.
	// +optional
	EnvironmentDefaults *EnvironmentDefaults `json:"environmentDefaults,omitempty"`
}

// DefaultMaxEphemeralEnvironments is the quota a tenant that never mentioned one
// gets.
//
// It exists twice: here, and as the `+kubebuilder:default=3` marker on the field
// above, which is the one the API server applies. A marker cannot reference a
// constant, so the two are kept honest by a test that parses the generated CRD
// and compares them — a drift would show up as a quota of 3 in the cluster and 0
// in anything reading the object in Go.
const DefaultMaxEphemeralEnvironments int32 = 3

// EphemeralQuota is how many ephemeral environments this customer may hold.
//
// nil means the field was never set, which only happens to an object that did not
// come through the API server — a hand-built struct in a test, or a client that
// predates the field. Everything read from a cluster carries the default.
func (s *OdooTenantSpec) EphemeralQuota() int32 {
	if s.MaxEphemeralEnvironments == nil {
		return DefaultMaxEphemeralEnvironments
	}
	return *s.MaxEphemeralEnvironments
}

// Tenant condition types.
const (
	// ConditionWithinQuota is whether this customer can still open a throwaway
	// environment.
	//
	// A condition rather than a bare number because the reason is what somebody
	// acts on, and because it is the same answer the admission webhook gives — read
	// here in advance instead of discovered on a rejected apply.
	ConditionWithinQuota = "WithinQuota"
)

// OdooTenantStatus is written by the controller only.
type OdooTenantStatus struct {
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// Databases is how many OdooDatabase objects declare this tenant.
	// +optional
	Databases int32 `json:"databases,omitempty"`

	// EnvironmentHours is a MONOTONIC total of sized environment-hours consumed.
	//
	// Monotonic, and persisted here rather than derived from metrics, because the
	// distinction is what makes it usable at all: a gauge of what is open answers
	// "what is running", and an invoice needs "what was consumed". A counter that
	// resets when the operator redeploys cannot be invoiced from, and a Prometheus
	// retention window is not an accounting record.
	//
	// Sized, not raw: weighted by the size class already declared on each
	// environment, because an hour of `large` costs the platform about three times
	// an hour of `medium` and a meter that ignores that loses money on exactly the
	// customers who use the product most.
	//
	// Stored in milli-hours as an integer. Floats accumulate error over thousands
	// of additions and this value is only ever added to.
	// +optional
	EnvironmentMilliHours int64 `json:"environmentMilliHours,omitempty"`

	// LastAccountedAt is the watermark: consumption before it is already counted in
	// EnvironmentMilliHours.
	//
	// It is what makes the counter survive a restart without double counting, and
	// it bounds the known undercount — see the controller. Accounting that is
	// approximately right and says so beats accounting that is exactly wrong.
	// +optional
	LastAccountedAt *metav1.Time `json:"lastAccountedAt,omitempty"`

	// SharedDatabases is how many of those are Shared with other customers.
	//
	// Surfaced on purpose. It is not an error and it is not nothing: it is the
	// number that tells you how much of this customer's data cannot be handed
	// back to them without subsetting.
	// +optional
	SharedDatabases int32 `json:"sharedDatabases,omitempty"`

	// EphemeralEnvironments currently open, for the quota.
	// +optional
	EphemeralEnvironments int32 `json:"ephemeralEnvironments,omitempty"`

	// ImageStudies is what each catalogue entry turned out to contain, once the
	// operator ran it and asked.
	// +listType=map
	// +listMapKey=name
	// +optional
	ImageStudies []ImageStudy `json:"imageStudies,omitempty"`

	// +optional
	Message string `json:"message,omitempty"`
}

// OdooTenant is a customer.
//
// ImageCatalogueEntry is one image this customer may run.
type ImageCatalogueEntry struct {
	// Name is what a person picks from a list: "hms-18", not a registry path.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9.]*[a-z0-9])?$`
	Name string `json:"name"`

	// Image is the pullable reference.
	// +kubebuilder:validation:MinLength=1
	Image string `json:"image"`

	// OdooVersion this image is, for example "18.0".
	//
	// Declared rather than parsed out of the tag. A tag is a string somebody
	// chose: `hms:18` and `hms:18.0-rc2` and `hms:stable` may all be Odoo 18, and
	// guessing from the text is how a major upgrade gets waved through because
	// the tag did not look like one.
	// +kubebuilder:validation:Pattern=`^[0-9]+\.[0-9]+$`
	OdooVersion string `json:"odooVersion"`

	// Flavor is how the image is put together: the official odoo image, a Doodba
	// build, or something else. Declared once here rather than on every
	// environment that uses it.
	// +kubebuilder:default=Official
	// +optional
	Flavor ImageFlavor `json:"flavor,omitempty"`

	// Default marks the entry new environments get when none is named. Exactly
	// one entry may set it.
	// +optional
	Default bool `json:"default,omitempty"`

	// Notes is free text shown beside the name: "current production", "security
	// backports only".
	// +kubebuilder:validation:MaxLength=200
	// +optional
	Notes string `json:"notes,omitempty"`
}

// MajorUpgradeAck is the literal that authorises crossing a major version.
const MajorUpgradeAck = "i-accept-a-major-upgrade-rewrites-the-database-and-cannot-be-rolled-back"

// MajorUpgrade is the evidence for a major version change.
//
// +kubebuilder:validation:XValidation:rule="self.acknowledgement == 'i-accept-a-major-upgrade-rewrites-the-database-and-cannot-be-rolled-back'",message="majorUpgrade.acknowledgement must be set to its literal value: crossing a major version rewrites the database in place, and the way back is a restore from before it started, not a redeploy"
type MajorUpgrade struct {
	// ToImage is the catalogue entry name being promoted. Named explicitly so
	// this authorisation cannot be left behind and silently authorise the NEXT
	// major upgrade too.
	// +kubebuilder:validation:MinLength=1
	ToImage string `json:"toImage"`

	// RehearsalRef is an OdooRehearsal in this namespace that succeeded against
	// exactly the image being promoted.
	//
	// The webhook reads the object; it does not take your word for it. A
	// rehearsal that failed, that is still running, or that ran against a
	// different image is not evidence, and each of those is refused by name.
	// +kubebuilder:validation:MinLength=1
	RehearsalRef string `json:"rehearsalRef"`

	// +kubebuilder:validation:MinLength=1
	Acknowledgement string `json:"acknowledgement"`
}

// DefaultImage returns the catalogue entry new environments use.
func (s *OdooTenantSpec) DefaultImage() *ImageCatalogueEntry {
	for i := range s.Images {
		if s.Images[i].Default {
			return &s.Images[i]
		}
	}
	// A catalogue with no entry marked default still has an obvious answer when
	// it holds exactly one image, and refusing to use it would be pedantry.
	if len(s.Images) == 1 {
		return &s.Images[0]
	}
	return nil
}

// ImageByName finds a catalogue entry.
func (s *OdooTenantSpec) ImageByName(name string) *ImageCatalogueEntry {
	for i := range s.Images {
		if s.Images[i].Name == name {
			return &s.Images[i]
		}
	}
	return nil
}

// Major is the part of the version that cannot be crossed casually.
func (e *ImageCatalogueEntry) Major() string {
	if e == nil {
		return ""
	}
	major, _, _ := strings.Cut(e.OdooVersion, ".")
	return major
}

// EnvironmentDefaults are the parts of an OdooEnvironment that come from the
// customer rather than from the person opening it.
type EnvironmentDefaults struct {
	// Image is the Odoo image this customer's environments run.
	// +optional
	Image string `json:"image,omitempty"`

	// Database is where this customer's environments create their databases.
	// +optional
	Database *DatabaseSpec `json:"database,omitempty"`

	// Storage carries the filestore mode, which is a per-customer decision:
	// Database mode for a customer with few attachments, a ReadWriteMany claim
	// for one with many.
	// +optional
	Storage *StorageSpec `json:"storage,omitempty"`

	// Size is the default resource class.
	// +optional
	Size Size `json:"size,omitempty"`

	// Addons is the set of modules this customer's environments load.
	//
	// Declared once per customer rather than on every environment. A customer
	// runs the same handful of OCA repositories and the same private one across
	// every review environment they open; repeating that on each is how one of
	// them ends up with a different set and nobody notices until a module is
	// missing in staging and present in production.
	//
	// An environment that declares its own addons keeps them: a consultant
	// testing a branch of one repository should not have to restate the other
	// six, but they must be able to.
	// +optional
	Addons *AddonsSpec `json:"addons,omitempty"`
}

// ─────────────── What an image actually contains ───────────────
//
// The catalogue says a name, a registry reference and a version, and all three
// are things a person typed. None of them is evidence.
//
// The questions that get asked about an image are: which Odoo is really in there,
// which modules does it ship, which user does it run as, and does it have the
// tools a restore needs. Every one has been answered wrong at some point in this
// project by reading a tag or trusting a memory — the official image runs as uid
// 100 and Doodba's uid 100 is `messagebus`; Doodba's published base ships the
// scaffolding and not Odoo at all.
//
// So the operator runs the image and asks it, and writes the answer to the
// catalogue entry's status, where it outlives the Job that produced it.
//
// It is a REPORT and not a gate. An image whose study failed is not refused: the
// study runs the image, and an image that will not start is a fact worth showing
// rather than a reason to block somebody who knows what they are doing.

// ImageStudy is what an image turned out to contain.
type ImageStudy struct {
	// Name matches the catalogue entry.
	Name string `json:"name"`

	// Image is the reference that was studied, so a report cannot be read as
	// being about a different build after the entry is repointed.
	// +optional
	Image string `json:"image,omitempty"`

	// OdooVersion is what the image REPORTS, which may differ from what the
	// catalogue entry claims. That disagreement is the most useful thing this
	// produces.
	// +optional
	OdooVersion string `json:"odooVersion,omitempty"`

	// User is the uid and gid it runs as, and whether that user exists at all.
	// Odoo calls getpwuid at startup and dies if it does not.
	// +optional
	User string `json:"user,omitempty"`

	// AddonsPaths are the directories Odoo will search.
	// +optional
	AddonsPaths []string `json:"addonsPaths,omitempty"`

	// Modules is how many addons the image ships.
	// +optional
	Modules int32 `json:"modules,omitempty"`

	// ExtraModules are the addons beyond the ones Odoo itself ships — the answer
	// to "what is in this build". Truncated: the point is to recognise a build,
	// not to enumerate it.
	// +optional
	ExtraModules []string `json:"extraModules,omitempty"`

	// HasClickOdoo says whether click-odoo-contrib is installed. Snapshot
	// restores and moving the filestore into the database both need it.
	// +optional
	HasClickOdoo bool `json:"hasClickOdoo,omitempty"`

	// Flavor is what the layout looks like — reported, never used to override
	// what was declared. A study that silently corrected the declaration would
	// remove the check that the declaration is right.
	// +optional
	Flavor ImageFlavor `json:"flavor,omitempty"`

	// Findings are the things worth saying out loud, in words.
	// +optional
	Findings []string `json:"findings,omitempty"`

	// StudiedAt is when. An old report about a tag that moves is worse than no
	// report, so it is shown beside every figure.
	// +optional
	StudiedAt *metav1.Time `json:"studiedAt,omitempty"`

	// Failed says the study could not complete, and why.
	// +optional
	Failed string `json:"failed,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=oten
// +kubebuilder:printcolumn:name="Customer",type=string,JSONPath=`.spec.displayName`
// +kubebuilder:printcolumn:name="Odoo",type=string,JSONPath=`.spec.odooVersion`
// +kubebuilder:printcolumn:name="Release",type=string,JSONPath=`.spec.productRelease`
// +kubebuilder:printcolumn:name="DBs",type=integer,JSONPath=`.status.databases`
// +kubebuilder:printcolumn:name="Shared",type=integer,JSONPath=`.status.sharedDatabases`,description="Databases shared with other customers"
// +kubebuilder:printcolumn:name="Envs",type=integer,JSONPath=`.status.ephemeralEnvironments`
type OdooTenant struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   OdooTenantSpec   `json:"spec,omitempty"`
	Status OdooTenantStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// OdooTenantList is a list of OdooTenant.
type OdooTenantList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []OdooTenant `json:"items"`
}

func init() {
	SchemeBuilder.Register(&OdooTenant{}, &OdooTenantList{})
}

// IssuerKindAndName splits spec.certIssuer.
func (s *OdooTenantSpec) IssuerKindAndName() (kind, name string) {
	kind, name, _ = strings.Cut(s.CertIssuer, "/")
	return kind, name
}

// IsDefault reports whether this is the customer record used when none is named.
func (s *OdooTenantSpec) IsDefault() bool {
	return s.Default != nil && *s.Default
}
