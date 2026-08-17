// SPDX-License-Identifier: Apache-2.0
// Copyright (C) 2026 Antoni Romera

package v1alpha1

import (
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

	// +optional
	Message string `json:"message,omitempty"`
}

// OdooTenant is a customer.
//
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
