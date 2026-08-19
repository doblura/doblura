// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Antoni Romera

package controller

import (
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	doblurav1alpha1 "github.com/doblura/doblura/api/v1alpha1"
)

func dbWith(companies ...doblurav1alpha1.TenantCompany) *doblurav1alpha1.OdooDatabase {
	db := &doblurav1alpha1.OdooDatabase{}
	db.Name = "acme-qa"
	db.Namespace = "demo"
	db.Spec.Name = "acme_qa"
	db.Spec.Role = doblurav1alpha1.RoleQA
	db.Spec.Tenancy = doblurav1alpha1.TenancySingleTenant
	db.Spec.Companies = companies
	return db
}

// A database holding two customers is safe to hand to neither, and says which.
//
// The condition exists to be read at a glance before somebody gives a copy to a
// customer. "False" with no names would send them looking through the spec for
// who else is in there, at the moment they are least inclined to look.
func TestADatabaseWithTwoCustomersIsSafeForNeither(t *testing.T) {
	r := &OdooDatabaseReconciler{}
	db := dbWith(
		doblurav1alpha1.TenantCompany{Company: "Acme SL", TenantRef: "acme"},
		doblurav1alpha1.TenantCompany{Company: "Otra SA", TenantRef: "otra"},
	)
	st := db.Status.DeepCopy()

	r.handover(db, st)

	c := meta.FindStatusCondition(st.Conditions, doblurav1alpha1.ConditionHandoverSafe)
	if c == nil {
		t.Fatal("no handover condition was recorded at all, so the page that " +
			"asks 'can I give this to the customer' has no answer")
	}
	if c.Status != metav1.ConditionFalse {
		t.Fatalf("a database holding two customers reports handover as %s", c.Status)
	}
	for _, who := range []string{"acme", "otra"} {
		if !strings.Contains(c.Message, who) {
			t.Errorf("the message does not name %s: %q", who, c.Message)
		}
	}
}

// A database that declares nobody is safe for nobody.
//
// Not silently true. An empty company list means who is inside is unknown, and
// unknown must never read as safe on the one screen somebody checks before
// handing over data.
func TestADatabaseThatDeclaresNobodyIsNotSafe(t *testing.T) {
	r := &OdooDatabaseReconciler{}
	db := dbWith()
	st := db.Status.DeepCopy()

	r.handover(db, st)

	c := meta.FindStatusCondition(st.Conditions, doblurav1alpha1.ConditionHandoverSafe)
	if c == nil || c.Status != metav1.ConditionFalse {
		t.Fatalf("a database with no declared companies reports %v", c)
	}
	if c.Reason != "Undeclared" {
		t.Errorf("the reason is %q, which does not say that the list is empty", c.Reason)
	}
}

// A placement is a decision and is never revisited.
//
// Re-running the choice on every reconcile would move a database to whichever
// instance had the most room this minute — and the database does not move, so the
// status would simply start naming the wrong server.
func TestAPlacementIsNeverRevisited(t *testing.T) {
	r := &OdooDatabaseReconciler{}
	db := dbWith(doblurav1alpha1.TenantCompany{Company: "Acme SL", TenantRef: "acme"})
	st := db.Status.DeepCopy()
	st.PlacedOn = "pg-uno"

	// No client is set, so any attempt to list instances would panic. Reaching
	// the end without one IS the assertion.
	r.place(nil, db, st) //nolint:staticcheck // a nil context is fine: nothing uses it

	if st.PlacedOn != "pg-uno" {
		t.Fatalf("an already-placed database was moved to %q", st.PlacedOn)
	}
}
