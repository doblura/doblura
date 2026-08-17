// SPDX-License-Identifier: Apache-2.0
// Copyright (C) 2026 Antoni Romera

package v1alpha1

import (
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func reachable(n string, tier InstanceTier, avail, used, max int32) PlacementCandidate {
	return PlacementCandidate{
		Name: n,
		Spec: OdooInstanceSpec{Tier: tier, Capacity: InstanceCapacity{MaxDatabases: max}},
		Status: OdooInstanceStatus{
			Available: avail, Databases: used,
			Conditions: []metav1.Condition{{Type: ConditionReachable, Status: metav1.ConditionTrue}},
		},
	}
}

// Among the instances that qualify, the emptiest wins. Not first-fit: spreading
// load is what stops the "instance full" call landing on the busiest server.
func TestPlacePicksTheEmptiest(t *testing.T) {
	got, err := Place(&OdooDatabaseSpec{Role: RoleStaging}, []PlacementCandidate{
		reachable("np-1", TierNonProduction, 2, 18, 20),
		reachable("np-2", TierNonProduction, 15, 5, 20),
		reachable("np-3", TierNonProduction, 9, 11, 20),
	})
	if err != nil {
		t.Fatal(err)
	}
	if got != "np-2" {
		t.Errorf("placed on %q, expected np-2 (the emptiest)", got)
	}
}

// The same catalogue must always yield the same answer, whatever order it
// arrives in: two reconciling operators cannot disagree.
func TestPlaceIsDeterministic(t *testing.T) {
	a := reachable("np-a", TierNonProduction, 10, 10, 20)
	b := reachable("np-b", TierNonProduction, 10, 10, 20)
	first, err := Place(&OdooDatabaseSpec{Role: RoleQA}, []PlacementCandidate{a, b})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 20; i++ {
		if got, _ := Place(&OdooDatabaseSpec{Role: RoleQA}, []PlacementCandidate{b, a}); got != first {
			t.Fatalf("order changed the decision: %q then %q", first, got)
		}
	}
}

// When nothing qualifies, the refusal names every instance and why each one was
// rejected. "no instance available" on its own sends people digging through YAML.
//
// The role here is Staging so the tier check passes and the OTHER reasons get a
// chance to surface — with a Production role the tier rejection would mask them
// all, which is correct behaviour and a different test.
func TestPlaceExplainsEveryRejection(t *testing.T) {
	cordoned := reachable("np-cordoned", TierNonProduction, 10, 0, 20)
	yes := true
	cordoned.Spec.Unschedulable = &yes

	full := reachable("np-full", TierNonProduction, 0, 20, 20)

	down := reachable("np-down", TierNonProduction, 10, 0, 20)
	down.Status.Conditions[0].Status = metav1.ConditionFalse

	_, err := Place(&OdooDatabaseSpec{Role: RoleStaging},
		[]PlacementCandidate{cordoned, full, down})
	if err == nil {
		t.Fatal("nothing qualified, so this must fail")
	}
	msg := err.Error()
	for _, want := range []string{"cordoned", "at capacity", "not reachable"} {
		if !strings.Contains(msg, want) {
			t.Errorf("the error should explain %q; got:\n%s", want, msg)
		}
	}
	for _, want := range []string{"np-cordoned", "np-full", "np-down"} {
		if !strings.Contains(msg, want) {
			t.Errorf("the error should name %q", want)
		}
	}
}

// A production database must not land on a non-production instance, and the tier
// rejection is the one that dominates: it is a policy refusal, not a capacity
// problem, so it is the answer the user needs first.
func TestPlaceRefusesProductionOnNonProduction(t *testing.T) {
	_, err := Place(&OdooDatabaseSpec{Role: RoleProduction}, []PlacementCandidate{
		reachable("np-1", TierNonProduction, 20, 0, 20),
		reachable("np-2", TierNonProduction, 20, 0, 20),
	})
	if err == nil {
		t.Fatal("production must not be placed on a non-production instance")
	}
	if !strings.Contains(err.Error(), "does not accept a Production database") {
		t.Errorf("got: %s", err)
	}

	// And with a production-tier instance in the catalogue it lands there.
	got, err := Place(&OdooDatabaseSpec{Role: RoleProduction}, []PlacementCandidate{
		reachable("np-1", TierNonProduction, 20, 0, 20),
		reachable("prod-1", TierProduction, 5, 15, 20),
	})
	if err != nil {
		t.Fatal(err)
	}
	if got != "prod-1" {
		t.Errorf("placed on %q, expected prod-1 even though it has less room", got)
	}
}

// An empty catalogue is a distinct case and deserves a distinct message.
func TestPlaceWithNoCandidates(t *testing.T) {
	_, err := Place(&OdooDatabaseSpec{Role: RoleStaging}, nil)
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "no OdooInstance exists") {
		t.Errorf("got: %s", err)
	}
}
