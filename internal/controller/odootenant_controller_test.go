// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Antoni Romera

package controller

import (
	"context"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	doblurav1alpha1 "github.com/doblura/doblura/api/v1alpha1"
)

func newTenantTest(t *testing.T, objs ...runtime.Object) (*OdooTenantReconciler, types.NamespacedName) {
	t.Helper()
	s := instScheme(t)
	c := fake.NewClientBuilder().
		WithScheme(s).
		WithRuntimeObjects(objs...).
		WithStatusSubresource(&doblurav1alpha1.OdooTenant{}).
		Build()
	return &OdooTenantReconciler{Client: c, Scheme: s, AccountEvery: time.Minute},
		types.NamespacedName{Namespace: "d", Name: "acme"}
}

func aTenant() *doblurav1alpha1.OdooTenant {
	return &doblurav1alpha1.OdooTenant{
		ObjectMeta: metav1.ObjectMeta{Name: "acme", Namespace: "d"},
	}
}

// anEnv builds an ephemeral environment for acme, ready `readyAgo` ago.
func anEnv(name string, size doblurav1alpha1.Size, readyAgo time.Duration) *doblurav1alpha1.OdooEnvironment {
	ttl := metav1.Duration{Duration: 8 * time.Hour}
	ready := metav1.NewTime(time.Now().Add(-readyAgo))
	return &doblurav1alpha1.OdooEnvironment{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "d"},
		Spec: doblurav1alpha1.OdooEnvironmentSpec{
			Image: "img", ForTenant: "acme", Size: size,
			Database:  doblurav1alpha1.DatabaseSpec{Host: "h", User: "u", PasswordSecret: "s"},
			Lifecycle: doblurav1alpha1.EnvLifecycle{TTL: &ttl},
		},
		Status: doblurav1alpha1.OdooEnvironmentStatus{ReadyAt: &ready},
	}
}

// ─────────────── the counter ───────────────

func TestFirstPassEstablishesTheWatermarkAndChargesNothing(t *testing.T) {
	// Accruing from ReadyAt on the first pass would silently invoice history that
	// predates the meter — including environments that were running before anyone
	// installed this controller.
	r, key := newTenantTest(t, aTenant(), anEnv("e1", doblurav1alpha1.SizeMedium, 5*time.Hour))
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key}); err != nil {
		t.Fatal(err)
	}
	var got doblurav1alpha1.OdooTenant
	if err := r.Get(context.Background(), key, &got); err != nil {
		t.Fatal(err)
	}
	if got.Status.EnvironmentMilliHours != 0 {
		t.Errorf("charged %d milli-hours on the first pass; want 0",
			got.Status.EnvironmentMilliHours)
	}
	if got.Status.LastAccountedAt == nil {
		t.Fatal("the watermark must be established so the next pass can accrue")
	}
}

func TestAccrualIsWeightedBySizeAndOnlyMonotonic(t *testing.T) {
	tenant := aTenant()
	// Watermark one hour ago: one hour of each environment is billable.
	hourAgo := metav1.NewTime(time.Now().Add(-time.Hour))
	tenant.Status.LastAccountedAt = &hourAgo
	tenant.Status.EnvironmentMilliHours = 5_000 // 5 hours already on the clock

	r, key := newTenantTest(t, tenant,
		anEnv("small", doblurav1alpha1.SizeSmall, 10*time.Hour),
		anEnv("medium", doblurav1alpha1.SizeMedium, 10*time.Hour),
		anEnv("large", doblurav1alpha1.SizeLarge, 10*time.Hour),
	)
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key}); err != nil {
		t.Fatal(err)
	}
	var got doblurav1alpha1.OdooTenant
	if err := r.Get(context.Background(), key, &got); err != nil {
		t.Fatal(err)
	}
	// 0.5 + 1 + 3 = 4.5 sized hours, on top of the 5 already recorded.
	const want = 5_000 + 4_500
	if d := got.Status.EnvironmentMilliHours - want; d > 20 || d < -20 {
		t.Errorf("milliHours = %d, want ~%d (0.5+1+3 weighting)",
			got.Status.EnvironmentMilliHours, want)
	}
	if got.Status.EnvironmentMilliHours < 5_000 {
		t.Error("the counter went backwards; it is only ever added to")
	}
}

func TestAccrualStopsAtTerminatedAt(t *testing.T) {
	// The reason terminatedAt is recorded before the object is deleted: without it
	// the watermark keeps accruing against an environment that stopped.
	tenant := aTenant()
	twoAgo := metav1.NewTime(time.Now().Add(-2 * time.Hour))
	tenant.Status.LastAccountedAt = &twoAgo

	env := anEnv("e1", doblurav1alpha1.SizeMedium, 3*time.Hour)
	stopped := metav1.NewTime(time.Now().Add(-time.Hour)) // ran for one of the two
	env.Status.TerminatedAt = &stopped

	r, key := newTenantTest(t, tenant, env)
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key}); err != nil {
		t.Fatal(err)
	}
	var got doblurav1alpha1.OdooTenant
	if err := r.Get(context.Background(), key, &got); err != nil {
		t.Fatal(err)
	}
	if d := got.Status.EnvironmentMilliHours - 1_000; d > 20 || d < -20 {
		t.Errorf("milliHours = %d, want ~1000: it ran for one of the two hours",
			got.Status.EnvironmentMilliHours)
	}
}

func TestNothingAccruesBeforeReady(t *testing.T) {
	// Restoring 40 GiB takes minutes to hours and nobody should be billed for a
	// snapshot they could not open.
	tenant := aTenant()
	hourAgo := metav1.NewTime(time.Now().Add(-time.Hour))
	tenant.Status.LastAccountedAt = &hourAgo

	restoring := anEnv("e1", doblurav1alpha1.SizeLarge, 0)
	restoring.Status.ReadyAt = nil // still restoring

	r, key := newTenantTest(t, tenant, restoring)
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key}); err != nil {
		t.Fatal(err)
	}
	var got doblurav1alpha1.OdooTenant
	if err := r.Get(context.Background(), key, &got); err != nil {
		t.Fatal(err)
	}
	if got.Status.EnvironmentMilliHours != 0 {
		t.Errorf("charged %d for an environment that was never ready",
			got.Status.EnvironmentMilliHours)
	}
}

func TestAccrualIgnoresOtherTenants(t *testing.T) {
	tenant := aTenant()
	hourAgo := metav1.NewTime(time.Now().Add(-time.Hour))
	tenant.Status.LastAccountedAt = &hourAgo

	theirs := anEnv("theirs", doblurav1alpha1.SizeLarge, 5*time.Hour)
	theirs.Spec.ForTenant = "globex"

	r, key := newTenantTest(t, tenant, anEnv("mine", doblurav1alpha1.SizeMedium, 5*time.Hour), theirs)
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key}); err != nil {
		t.Fatal(err)
	}
	var got doblurav1alpha1.OdooTenant
	if err := r.Get(context.Background(), key, &got); err != nil {
		t.Fatal(err)
	}
	// One medium hour only. Globex's large environment must not appear on acme's
	// bill, which is the least forgivable possible bug in this file.
	if d := got.Status.EnvironmentMilliHours - 1_000; d > 20 || d < -20 {
		t.Errorf("milliHours = %d, want ~1000 — another tenant's usage leaked in",
			got.Status.EnvironmentMilliHours)
	}
}

func TestCounterSurvivesRepeatedPasses(t *testing.T) {
	// Restart-safety, expressed as the property that matters: many passes must not
	// multiply the charge for the same elapsed time.
	tenant := aTenant()
	start := metav1.NewTime(time.Now().Add(-time.Hour))
	tenant.Status.LastAccountedAt = &start

	r, key := newTenantTest(t, tenant, anEnv("e1", doblurav1alpha1.SizeMedium, 5*time.Hour))
	ctx := context.Background()
	for i := 0; i < 5; i++ {
		if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: key}); err != nil {
			t.Fatal(err)
		}
	}
	var got doblurav1alpha1.OdooTenant
	if err := r.Get(ctx, key, &got); err != nil {
		t.Fatal(err)
	}
	// One hour elapsed in total, however many times we looked at it.
	if got.Status.EnvironmentMilliHours > 1_100 {
		t.Errorf("milliHours = %d after five passes over one hour; the watermark is not holding",
			got.Status.EnvironmentMilliHours)
	}
}

// ─────────────── the counts ───────────────

func TestOpenEphemeralExcludesPersistentAndTerminated(t *testing.T) {
	persistent := anEnv("staging", doblurav1alpha1.SizeMedium, time.Hour)
	persistent.Spec.Lifecycle.TTL = nil // staging is not a throwaway

	gone := anEnv("gone", doblurav1alpha1.SizeMedium, time.Hour)
	stopped := metav1.Now()
	gone.Status.TerminatedAt = &stopped

	r, key := newTenantTest(t, aTenant(),
		anEnv("open-1", doblurav1alpha1.SizeMedium, time.Hour),
		anEnv("open-2", doblurav1alpha1.SizeSmall, time.Hour),
		persistent, gone,
	)
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key}); err != nil {
		t.Fatal(err)
	}
	var got doblurav1alpha1.OdooTenant
	if err := r.Get(context.Background(), key, &got); err != nil {
		t.Fatal(err)
	}
	if got.Status.EphemeralEnvironments != 2 {
		t.Errorf("ephemeralEnvironments = %d, want 2 (persistent and terminated do not count)",
			got.Status.EphemeralEnvironments)
	}
}

func TestQuotaConditionGoesFalseAtTheLimit(t *testing.T) {
	// Three is the default quota, so three open environments is at the limit — the
	// same answer the admission webhook gives, readable in advance.
	r, key := newTenantTest(t, aTenant(),
		anEnv("a", doblurav1alpha1.SizeSmall, time.Hour),
		anEnv("b", doblurav1alpha1.SizeSmall, time.Hour),
		anEnv("c", doblurav1alpha1.SizeSmall, time.Hour),
	)
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key}); err != nil {
		t.Fatal(err)
	}
	var got doblurav1alpha1.OdooTenant
	if err := r.Get(context.Background(), key, &got); err != nil {
		t.Fatal(err)
	}
	c := findLinkCond(got.Status.Conditions, doblurav1alpha1.ConditionWithinQuota)
	if c == nil || c.Status != metav1.ConditionFalse || c.Reason != "AtQuota" {
		t.Fatalf("WithinQuota should be False at the limit: %+v", c)
	}
	if c.Message != "3 of 3 ephemeral environments open" {
		t.Errorf("message = %q", c.Message)
	}
}

func TestSharedDatabasesAreCounted(t *testing.T) {
	shared := &doblurav1alpha1.OdooDatabase{
		ObjectMeta: metav1.ObjectMeta{Name: "shared", Namespace: "d"},
		Spec: doblurav1alpha1.OdooDatabaseSpec{
			Name: "shared", Role: doblurav1alpha1.RoleProduction,
			Tenancy: doblurav1alpha1.TenancyShared,
			Companies: []doblurav1alpha1.TenantCompany{
				{Company: "Acme SA", TenantRef: "acme"},
				{Company: "Globex GmbH", TenantRef: "globex"},
			},
		},
	}
	own := &doblurav1alpha1.OdooDatabase{
		ObjectMeta: metav1.ObjectMeta{Name: "own", Namespace: "d"},
		Spec: doblurav1alpha1.OdooDatabaseSpec{
			Name: "own", Role: doblurav1alpha1.RoleProduction,
			Tenancy:   doblurav1alpha1.TenancySingleTenant,
			Companies: []doblurav1alpha1.TenantCompany{{Company: "Acme SA", TenantRef: "acme"}},
		},
	}
	r, key := newTenantTest(t, aTenant(), shared, own)
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key}); err != nil {
		t.Fatal(err)
	}
	var got doblurav1alpha1.OdooTenant
	if err := r.Get(context.Background(), key, &got); err != nil {
		t.Fatal(err)
	}
	if got.Status.Databases != 2 || got.Status.SharedDatabases != 1 {
		t.Errorf("databases=%d shared=%d, want 2 and 1",
			got.Status.Databases, got.Status.SharedDatabases)
	}
}
