// SPDX-License-Identifier: Apache-2.0
// Copyright (C) 2026 Antoni Romera

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ─────────────── The database, and who is inside it ───────────────
//
// The plan called for OdooInstance and OdooTenant. Writing them made it obvious
// that a third type is needed, and it is worth recording why rather than
// pretending it was always there.
//
// A single Odoo database can hold several customers, as separate res.company
// records. So the relation between customer and database is many-to-many. If
// each OdooTenant listed its own databases, a database shared by three customers
// would be declared three times, and the three declarations could disagree about
// its tenancy. There would be no single place to answer the one question that
// matters:
//
//	"can a copy of this database be handed to one of its customers?"
//
// Tenancy is a property of the database, not of the customer. So it lives here.
//
// The three cases, and only the third is dangerous
// ────────────────────────────────────────────────
//
//	SingleTenant      one customer. Safe to copy and hand over.
//	RelatedCompanies  several companies of the SAME customer — a group with
//	                  legal entities in several countries. Safe: it is exactly
//	                  what Odoo's multi-company feature is for.
//	Shared            companies of UNRELATED customers. Copying it hands each
//	                  one the others' business data.
//
// Odoo puts company_id on almost every transactional record, but it shares the
// master data on purpose: contacts, products, users. So even a perfect
// company_id filter leaves the res_partner rows the customer trades with — and
// some of those are also the other customers' contacts. It is literally the same
// row. No filter fixes that, and the platform's job is to say so rather than
// pretend.

// DatabaseTenancy declares who is inside a database.
// +kubebuilder:validation:Enum=SingleTenant;RelatedCompanies;Shared
type DatabaseTenancy string

const (
	// TenancySingleTenant is one customer, one database. The simple case.
	TenancySingleTenant DatabaseTenancy = "SingleTenant"

	// TenancyRelatedCompanies is several Odoo companies belonging to the same
	// customer. Safe, and what multi-company exists for.
	TenancyRelatedCompanies DatabaseTenancy = "RelatedCompanies"

	// TenancyShared is companies belonging to unrelated customers.
	//
	// Declaring this does not make it safe. It makes it visible, which is the
	// most the platform can honestly do: it will refuse to build a
	// customer-facing environment from this database unless subsetting is
	// configured, and even then the shared master data is a caveat you own.
	TenancyShared DatabaseTenancy = "Shared"
)

// DatabaseRole is what a database is for. It drives placement.
// +kubebuilder:validation:Enum=Production;Staging;QA;Review;Rehearsal;Development
type DatabaseRole string

const (
	RoleProduction  DatabaseRole = "Production"
	RoleStaging     DatabaseRole = "Staging"
	RoleQA          DatabaseRole = "QA"
	RoleReview      DatabaseRole = "Review"
	RoleRehearsal   DatabaseRole = "Rehearsal"
	RoleDevelopment DatabaseRole = "Development"
)

// TenantCompany maps an Odoo company to the customer that owns it.
//
// The lengths are bounded, and not for tidiness. The CEL rules below iterate over
// this list, and the API server estimates a rule's cost from the DECLARED bounds
// of what it walks. Unbounded strings and an unbounded list make the estimate
// enormous and the whole CRD is refused at install time with "estimated rule cost
// exceeds budget". Declaring realistic maxima is what makes the validation
// affordable.
type TenantCompany struct {
	// Company is the res.company name as it appears in the database.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=128
	Company string `json:"company"`

	// TenantRef is the OdooTenant that owns this company.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	TenantRef string `json:"tenantRef"`
}

// OdooDatabaseSpec describes one Odoo database.
//
// The tenancy declaration has to agree with the companies listed. These rules
// are the reason the type exists, so the API server enforces them:
//
// Every rule guards has(self.companies) first. Without it, `size(self.companies)`
// on an object that simply omitted the field is not false — it is an evaluation
// ERROR ("no such key"), and the API server rejects a perfectly valid
// SingleTenant database that named no companies at all. The same trap caught
// OdooEnvironment's security block earlier.
//
// +kubebuilder:validation:XValidation:rule="self.tenancy != 'SingleTenant' || !has(self.companies) || size(self.companies) <= 1",message="tenancy SingleTenant cannot list more than one company; use RelatedCompanies if they belong to the same customer, or Shared if they do not"
// +kubebuilder:validation:XValidation:rule="self.tenancy != 'RelatedCompanies' || !has(self.companies) || self.companies.all(c, c.tenantRef == self.companies[0].tenantRef)",message="tenancy RelatedCompanies requires every company to belong to the SAME tenant; if the tenants differ this database is Shared"
// +kubebuilder:validation:XValidation:rule="self.tenancy != 'Shared' || (has(self.companies) && size(self.companies) >= 2)",message="tenancy Shared means unrelated customers in one database, so it must list at least two companies"
type OdooDatabaseSpec struct {
	// Name is the database name on the server.
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`

	// InstanceRef is the OdooInstance this database lives on. Empty means the
	// database is not placed yet and the placer will choose.
	// +optional
	InstanceRef string `json:"instanceRef,omitempty"`

	// Role is what the database is for. It decides which instance tiers may
	// host it.
	Role DatabaseRole `json:"role"`

	// Tenancy declares who is inside. Read the type comment before choosing
	// Shared.
	// +kubebuilder:default=SingleTenant
	// +optional
	Tenancy DatabaseTenancy `json:"tenancy,omitempty"`

	// Companies maps the Odoo companies in this database to their owners.
	//
	// For SingleTenant it can be omitted or hold one entry. For anything else it
	// is what makes the tenancy claim checkable, and what subsetting needs in
	// order to know which company to extract.
	// A database with more than 64 companies in it is not a multi-company setup,
	// it is a hosting platform, and it needs a different conversation. The bound
	// also keeps the CEL rules above within the API server's cost budget.
	// +kubebuilder:validation:MaxItems=64
	// +optional
	Companies []TenantCompany `json:"companies,omitempty"`

	// SizeGi is the approximate size, used by placement to reserve headroom.
	// +optional
	SizeGi *int32 `json:"sizeGi,omitempty"`
}

// OdooDatabaseStatus is written by the controller only.
type OdooDatabaseStatus struct {
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// PlacedOn is the instance the placer chose, once chosen. It is recorded in
	// the status rather than written back into the spec: the spec is the user's
	// intent and the placement is the platform's decision.
	// +optional
	PlacedOn string `json:"placedOn,omitempty"`

	// Exists is whether the database was actually observed on the server.
	// +optional
	Exists bool `json:"exists,omitempty"`

	// +optional
	Message string `json:"message,omitempty"`
}

// Database condition types.
const (
	// ConditionPlaced is whether an instance has been chosen.
	ConditionPlaced = "Placed"
	// ConditionHandoverSafe is whether a copy of this database may be given to
	// one of its customers.
	//
	// A condition rather than a computed field because it is the answer people
	// need at a glance, and because the reason matters as much as the answer.
	ConditionHandoverSafe = "HandoverSafe"
)

// OdooDatabase is one Odoo database: where it lives, what it is for, and who is
// inside it.
//
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=odb
// +kubebuilder:printcolumn:name="DB",type=string,JSONPath=`.spec.name`
// +kubebuilder:printcolumn:name="Role",type=string,JSONPath=`.spec.role`
// +kubebuilder:printcolumn:name="Tenancy",type=string,JSONPath=`.spec.tenancy`
// +kubebuilder:printcolumn:name="Instance",type=string,JSONPath=`.status.placedOn`
// +kubebuilder:printcolumn:name="Handover",type=string,JSONPath=`.status.conditions[?(@.type=="HandoverSafe")].status`
type OdooDatabase struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   OdooDatabaseSpec   `json:"spec,omitempty"`
	Status OdooDatabaseStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// OdooDatabaseList is a list of OdooDatabase.
type OdooDatabaseList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []OdooDatabase `json:"items"`
}

func init() {
	SchemeBuilder.Register(&OdooDatabase{}, &OdooDatabaseList{})
}

// HandoverSafeFor reports whether a copy of this database may be handed to the
// given tenant, and why not when the answer is no.
//
// This is the single function the multi-tenancy guardrail rests on, which is why
// it lives in the API next to the types and is tested on its own rather than
// buried in a reconciler.
func (s *OdooDatabaseSpec) HandoverSafeFor(tenant string) (bool, string) {
	switch s.Tenancy {
	case TenancySingleTenant:
		return true, "single-tenant database"

	case TenancyRelatedCompanies:
		// Every company belongs to one customer, so a full copy reveals only
		// that customer's own data.
		for _, c := range s.Companies {
			if c.TenantRef != tenant {
				return false, "database holds companies of " + c.TenantRef +
					", not " + tenant + "; the RelatedCompanies declaration is wrong"
			}
		}
		return true, "all companies belong to the same customer"

	case TenancyShared:
		var others []string
		for _, c := range s.Companies {
			if c.TenantRef != tenant {
				others = append(others, c.TenantRef)
			}
		}
		if len(others) == 0 {
			// Declared Shared but nobody else is in it. Trust the declaration
			// over the list: whoever wrote Shared knew something.
			return false, "declared Shared; the company list may be incomplete"
		}
		return false, "database is shared with " + joinNames(others) +
			"; a full copy would hand them their data. Configure company subsetting, " +
			"and note that Odoo shares master data (contacts, products, users) " +
			"between companies by design, so a subset is not a clean separation"
	}
	return false, "unknown tenancy"
}

func joinNames(n []string) string {
	switch len(n) {
	case 0:
		return "nobody"
	case 1:
		return n[0]
	}
	out := n[0]
	for _, x := range n[1 : len(n)-1] {
		out += ", " + x
	}
	return out + " and " + n[len(n)-1]
}
