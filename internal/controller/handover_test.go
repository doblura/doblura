// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Antoni Romera

package controller

import (
	"context"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	doblurav1alpha1 "github.com/doblura/doblura/api/v1alpha1"
)

func scheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := doblurav1alpha1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	return s
}

func db(name string, tenancy doblurav1alpha1.DatabaseTenancy, companies ...doblurav1alpha1.TenantCompany) *doblurav1alpha1.OdooDatabase {
	return &doblurav1alpha1.OdooDatabase{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "demo"},
		Spec: doblurav1alpha1.OdooDatabaseSpec{
			Name: name, Role: doblurav1alpha1.RoleProduction,
			Tenancy: tenancy, Companies: companies,
		},
	}
}

// The guardrail this phase exists for: a shared database cannot be handed to one
// of its customers, and the refusal names the others.
func TestHandoverRefusesASharedDatabase(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(scheme(t)).WithObjects(
		db("prod-shared", doblurav1alpha1.TenancyShared,
			doblurav1alpha1.TenantCompany{Company: "Acme", TenantRef: "acme"},
			doblurav1alpha1.TenantCompany{Company: "Globex", TenantRef: "globex"}),
	).Build()

	d, err := CheckHandover(context.Background(), c, "demo", "prod-shared", "acme")
	if err != nil {
		t.Fatal(err)
	}
	if d.Allowed {
		t.Fatal("a shared database must not be handed to one of its customers")
	}
	if !strings.Contains(d.Reason, "globex") {
		t.Errorf("the refusal must name the other customer; got: %s", d.Reason)
	}
	if !strings.Contains(d.Reason, "master data") {
		t.Errorf("the refusal must carry the shared-master-data caveat; got: %s", d.Reason)
	}
}

func TestHandoverAllowsSingleTenantAndRelatedCompanies(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(scheme(t)).WithObjects(
		db("prod-acme", doblurav1alpha1.TenancySingleTenant),
		db("prod-group", doblurav1alpha1.TenancyRelatedCompanies,
			doblurav1alpha1.TenantCompany{Company: "Acme ES", TenantRef: "acme"},
			doblurav1alpha1.TenantCompany{Company: "Acme PT", TenantRef: "acme"}),
	).Build()

	for _, name := range []string{"prod-acme", "prod-group"} {
		d, err := CheckHandover(context.Background(), c, "demo", name, "acme")
		if err != nil {
			t.Fatal(err)
		}
		if !d.Allowed {
			t.Errorf("%s should be safe for acme: %s", name, d.Reason)
		}
	}
}

// Doblura has to stay usable before the catalogue is filled in. An unknown
// database is allowed, and says so — refusing on missing metadata would teach
// people to route around the guardrail.
func TestHandoverWithoutCatalogueSaysItCouldNotCheck(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(scheme(t)).Build()

	for _, tc := range []struct{ name, source, tenant, wants string }{
		{"unknown database", "nowhere", "acme", "does not exist"},
		{"no source named", "", "acme", "does not name an OdooDatabase"},
		{"no target customer", "prod", "", "not a handover"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d, err := CheckHandover(context.Background(), c, "demo", tc.source, tc.tenant)
			if err != nil {
				t.Fatal(err)
			}
			if !d.Allowed {
				t.Error("must not block when it cannot verify")
			}
			if !strings.Contains(d.Reason, tc.wants) {
				t.Errorf("reason should contain %q; got: %s", tc.wants, d.Reason)
			}
		})
	}
}
