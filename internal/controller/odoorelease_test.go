// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Antoni Romera

package controller

import (
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	doblurav1alpha1 "github.com/doblura/doblura/api/v1alpha1"
)

func release(size int32, soak time.Duration) *doblurav1alpha1.OdooRelease {
	return &doblurav1alpha1.OdooRelease{
		ObjectMeta: metav1.ObjectMeta{Name: "r2026-3", Namespace: "demo"},
		Spec: doblurav1alpha1.OdooReleaseSpec{
			Version: "2026.3",
			Batch: doblurav1alpha1.BatchPolicy{
				Size: &size, Soak: &metav1.Duration{Duration: soak},
			},
		},
	}
}

// The batch is a pace, and the soak is measured from the last customer moved.
//
// "Three at a time" and "and then wait a day" are the same rule said twice, and
// the second is what people mean by it: the point of a batch is that the first
// few customers find out what the release does to real data before the next few
// get it.
func TestABatchWaitsForItsSoak(t *testing.T) {
	rel := release(3, time.Hour)
	now := time.Now()

	fresh := &doblurav1alpha1.OdooReleaseStatus{}
	if batchRoom(rel, fresh, now) != 3 {
		t.Error("a rollout that has moved nobody has no room")
	}

	moving := &doblurav1alpha1.OdooReleaseStatus{
		LastMovedAt: &metav1.Time{Time: now.Add(-5 * time.Minute)},
	}
	if batchRoom(rel, moving, now) != 0 {
		t.Error("a batch that moved somebody five minutes ago is still soaking")
	}

	soaked := &doblurav1alpha1.OdooReleaseStatus{
		LastMovedAt: &metav1.Time{Time: now.Add(-2 * time.Hour)},
	}
	if batchRoom(rel, soaked, now) != 3 {
		t.Error("the next batch never starts")
	}
}

// The defaults are decisions, not placeholders.
func TestTheDefaultSoakIsADay(t *testing.T) {
	bare := &doblurav1alpha1.OdooRelease{}
	if soakOf(bare) != 24*time.Hour {
		t.Errorf("the default soak is %s: most of what a bad release does to an Odoo "+
			"shows up when somebody USES it, so an hour proves the pods are up and "+
			"very little else", soakOf(bare))
	}
	if batchRoom(bare, &doblurav1alpha1.OdooReleaseStatus{}, time.Now()) != 3 {
		t.Error("the default batch is not a few")
	}
}

// A release owns one catalogue entry, named after itself.
//
// After ITSELF and not after the version: a release that gets re-pointed updates
// its own entry instead of leaving a trail of them behind, and two releases
// cannot quietly share one entry.
func TestAReleaseOwnsOneCatalogueEntry(t *testing.T) {
	if got := releaseEntryName(release(1, time.Hour)); got != "release-r2026-3" {
		t.Errorf("the entry is called %q", got)
	}
}
