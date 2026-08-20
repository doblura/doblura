// SPDX-License-Identifier: Apache-2.0
// Copyright (C) 2026 Antoni Romera

package v1alpha1

import (
	"strings"
	"testing"
)

// The one question this whole type exists to answer: can a copy of this database
// be handed to one of its customers?
func TestHandoverSafety(t *testing.T) {
	cases := []struct {
		name    string
		spec    OdooDatabaseSpec
		tenant  string
		wantOK  bool
		wantHas string // substring the explanation must contain
	}{
		{
			name:   "single tenant is safe",
			spec:   OdooDatabaseSpec{Tenancy: TenancySingleTenant},
			tenant: "acme",
			wantOK: true,
		},
		{
			name: "related companies of the same customer is safe",
			spec: OdooDatabaseSpec{
				Tenancy: TenancyRelatedCompanies,
				Companies: []TenantCompany{
					{Company: "Acme ES", TenantRef: "acme"},
					{Company: "Acme PT", TenantRef: "acme"},
					{Company: "Acme FR", TenantRef: "acme"},
				},
			},
			tenant: "acme",
			wantOK: true,
		},
		{
			name: "RelatedCompanies that actually holds another customer is caught",
			spec: OdooDatabaseSpec{
				Tenancy: TenancyRelatedCompanies,
				Companies: []TenantCompany{
					{Company: "Acme ES", TenantRef: "acme"},
					{Company: "Globex", TenantRef: "globex"},
				},
			},
			tenant:  "acme",
			wantOK:  false,
			wantHas: "declaration is wrong",
		},
		{
			name: "shared names who else is in there",
			spec: OdooDatabaseSpec{
				Tenancy: TenancyShared,
				Companies: []TenantCompany{
					{Company: "Acme", TenantRef: "acme"},
					{Company: "Globex", TenantRef: "globex"},
					{Company: "Initech", TenantRef: "initech"},
				},
			},
			tenant:  "acme",
			wantOK:  false,
			wantHas: "globex and initech",
		},
		{
			name: "shared warns about master data even when subsetting is mentioned",
			spec: OdooDatabaseSpec{
				Tenancy:   TenancyShared,
				Companies: []TenantCompany{{Company: "A", TenantRef: "a"}, {Company: "B", TenantRef: "b"}},
			},
			tenant:  "a",
			wantOK:  false,
			wantHas: "master data",
		},
		{
			name:    "declared Shared with an incomplete list still refuses",
			spec:    OdooDatabaseSpec{Tenancy: TenancyShared},
			tenant:  "acme",
			wantOK:  false,
			wantHas: "incomplete",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ok, why := tc.spec.HandoverSafeFor(tc.tenant)
			if ok != tc.wantOK {
				t.Fatalf("safe = %v, expected %v (%s)", ok, tc.wantOK, why)
			}
			if tc.wantHas != "" && !strings.Contains(why, tc.wantHas) {
				t.Errorf("explanation should mention %q, got: %s", tc.wantHas, why)
			}
		})
	}
}

// Production must never land on an instance that also takes non-production
// workloads. It is the rule that keeps a rehearsal from competing for I/O with a
// customer who is invoicing.
func TestTierSeparation(t *testing.T) {
	prod := OdooInstanceSpec{Tier: TierProduction}
	nonprod := OdooInstanceSpec{Tier: TierNonProduction}
	any := OdooInstanceSpec{Tier: TierAny}

	cases := []struct {
		inst OdooInstanceSpec
		role DatabaseRole
		want bool
	}{
		{prod, RoleProduction, true},
		{prod, RoleStaging, false},
		{prod, RoleRehearsal, false},
		{nonprod, RoleProduction, false},
		{nonprod, RoleStaging, true},
		{nonprod, RoleReview, true},
		{any, RoleProduction, true},
		{any, RoleRehearsal, true},
	}
	for _, c := range cases {
		if got := c.inst.AcceptsRole(c.role); got != c.want {
			t.Errorf("tier %s accepting %s = %v, expected %v", c.inst.Tier, c.role, got, c.want)
		}
	}
}

// A catalogue entry that names a build resolves to what that build produced.
//
// One function, two callers — the environment webhook and the tenant's image
// study — because two implementations of "which image is this?" is how a study
// reports on one thing while an environment runs another.
func TestACatalogueEntryResolvesToWhatItsBuildProduced(t *testing.T) {
	digest := "reg/acme/erp@sha256:c0e9195ed79ba81e615743d91446b33d9ea22b38dafe4da346e23befe385dc5b"
	built := &OdooBuild{Status: OdooBuildStatus{
		Phase: BuildSucceeded, Image: digest,
	}}

	plain := &ImageCatalogueEntry{Name: "e", Image: "odoo:18.0"}
	if ref, why := plain.ResolveWith(nil); ref != "odoo:18.0" || why != "" {
		t.Errorf("an entry that names an image resolved to %q (%s)", ref, why)
	}

	fromBuild := &ImageCatalogueEntry{Name: "e", FromBuild: "erp-18"}
	ref, why := fromBuild.ResolveWith(built)
	if ref != digest || why != "" {
		t.Errorf("the built image did not reach the catalogue: %q (%s)", ref, why)
	}

	// A build that has not produced anything is a sentence, not an empty image:
	// an environment cannot run what has not been built, and "" as an image is
	// how a pod ends up pulling something nobody named.
	if ref, why := fromBuild.ResolveWith(&OdooBuild{
		Status: OdooBuildStatus{Phase: BuildBuilding, Message: "building"},
	}); ref != "" || why == "" {
		t.Errorf("an unfinished build resolved to %q with no reason", ref)
	}
	// And a build that is not there at all says so, rather than resolving to
	// nothing and letting the failure appear as a pull error three steps later.
	if ref, why := fromBuild.ResolveWith(nil); ref != "" || !strings.Contains(why, "no such build") {
		t.Errorf("a missing build gave %q / %q", ref, why)
	}
}
