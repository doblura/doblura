// SPDX-License-Identifier: Apache-2.0
// Copyright (C) 2026 Antoni Romera

package v1alpha1

import (
	"fmt"
	"sort"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ─────────────── Placement ───────────────
//
// Choosing which instance a database lands on. A pure function over the
// catalogue, deliberately: placement decisions are the kind of thing you want to
// be able to reason about and test without a cluster.
//
// Two rules cover almost everything:
//
//  1. Production never shares an instance with non-production. A rehearsal
//     restoring forty gigabytes must not compete for I/O with a customer who is
//     invoicing.
//  2. Among the instances that qualify, pick the one with the most room. Not
//     round-robin and not first-fit: spreading load is what keeps the 3am
//     "instance full" from happening on the busiest server.

// PlacementCandidate is an instance offered to the placer.
//
// It carries the spec and the observed status together because both matter: the
// spec says what is allowed, the status says what is actually possible.
type PlacementCandidate struct {
	Name   string
	Spec   OdooInstanceSpec
	Status OdooInstanceStatus
}

// ErrNoInstance is returned when nothing qualifies. It names the reason for each
// rejection, because "no instance available" on its own sends people digging
// through YAML.
type ErrNoInstance struct {
	Role     DatabaseRole
	Rejected []string
}

func (e *ErrNoInstance) Error() string {
	if len(e.Rejected) == 0 {
		return fmt.Sprintf("no OdooInstance exists that can host a %s database", e.Role)
	}
	msg := fmt.Sprintf("no OdooInstance can host a %s database:", e.Role)
	for _, r := range e.Rejected {
		msg += "\n  - " + r
	}
	return msg
}

// Place picks the instance for a database, or explains why none fits.
func Place(db *OdooDatabaseSpec, candidates []PlacementCandidate) (string, error) {
	// Stable order in, stable decision out: two operators reconciling the same
	// database must reach the same answer, and a map iteration would not.
	sorted := append([]PlacementCandidate(nil), candidates...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Name < sorted[j].Name })

	var rejected []string
	var best *PlacementCandidate
	for i := range sorted {
		c := &sorted[i]
		if why := placementReject(db, c); why != "" {
			rejected = append(rejected, c.Name+": "+why)
			continue
		}
		if best == nil || c.Status.Available > best.Status.Available {
			best = c
		}
	}
	if best == nil {
		return "", &ErrNoInstance{Role: db.Role, Rejected: rejected}
	}
	return best.Name, nil
}

// placementReject returns why a candidate does not qualify, or "" if it does.
func placementReject(db *OdooDatabaseSpec, c *PlacementCandidate) string {
	if c.Spec.Unschedulable != nil && *c.Spec.Unschedulable {
		return "cordoned (spec.unschedulable)"
	}
	if !c.Spec.AcceptsRole(db.Role) {
		return fmt.Sprintf("tier %s does not accept a %s database", c.Spec.Tier, db.Role)
	}
	// Unreachable instances are skipped, not fatal: one server being down should
	// not stop a rehearsal that could run somewhere else.
	if !conditionTrue(c.Status.Conditions, ConditionReachable) {
		return "not reachable"
	}
	if c.Status.Available <= 0 {
		return fmt.Sprintf("at capacity (%d/%d databases)",
			c.Status.Databases, c.Spec.Capacity.MaxDatabases)
	}
	return ""
}

func conditionTrue(conds []metav1.Condition, t string) bool {
	for _, c := range conds {
		if c.Type == t {
			return c.Status == "True"
		}
	}
	return false
}
