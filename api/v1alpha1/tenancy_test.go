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
