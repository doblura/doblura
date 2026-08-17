// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Antoni Romera

package controller

import (
	"context"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	doblurav1alpha1 "github.com/doblura/doblura/api/v1alpha1"
)

func instScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	if err := doblurav1alpha1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	return s
}

func newInstTest(t *testing.T, objs ...runtime.Object) (*OdooInstanceReconciler, types.NamespacedName) {
	t.Helper()
	s := instScheme(t)
	c := fake.NewClientBuilder().
		WithScheme(s).
		WithRuntimeObjects(objs...).
		WithStatusSubresource(&doblurav1alpha1.OdooInstance{}).
		Build()
	return &OdooInstanceReconciler{Client: c, Scheme: s, ProbeImage: "postgres:18-alpine"},
		types.NamespacedName{Namespace: "d", Name: "pg-1"}
}

func anInstance() *doblurav1alpha1.OdooInstance {
	return &doblurav1alpha1.OdooInstance{
		ObjectMeta: metav1.ObjectMeta{Name: "pg-1", Namespace: "d"},
		Spec: doblurav1alpha1.OdooInstanceSpec{
			Host: "pg.db.svc", AdminUser: "odoo", AdminPasswordSecret: "pg-app",
			Tier:     doblurav1alpha1.TierNonProduction,
			Capacity: doblurav1alpha1.InstanceCapacity{MaxDatabases: 20},
		},
	}
}

// finishedPod is the probe as the API server would present it after it exited.
func finishedPod(phase corev1.PodPhase, msg string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: probePodName("pg-1"), Namespace: "d"},
		Status: corev1.PodStatus{
			Phase: phase,
			ContainerStatuses: []corev1.ContainerStatus{{
				Name:  "probe",
				State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{Message: msg}},
			}},
		},
	}
}

func TestInstanceFirstPassCreatesTheProbe(t *testing.T) {
	r, key := newInstTest(t, anInstance())
	res, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key})
	if err != nil {
		t.Fatal(err)
	}
	// It must come back soon, not in ten minutes: the probe finishes in seconds and
	// the observation is what everything else waits for.
	if res.RequeueAfter > 30*time.Second {
		t.Errorf("requeued in %v after creating the probe; should be seconds", res.RequeueAfter)
	}
	var pod corev1.Pod
	if err := r.Get(context.Background(), types.NamespacedName{
		Namespace: "d", Name: probePodName("pg-1")}, &pod); err != nil {
		t.Fatalf("no probe Pod was created: %v", err)
	}
	if pod.Spec.Containers[0].TerminationMessagePath != probeTerminationPath {
		t.Error("the probe must report through its termination message")
	}
	if pod.Spec.Containers[0].Env[0].Name != "PGHOST" {
		t.Error("the probe needs PGHOST")
	}
	// The credential must reach the Pod and nothing else.
	var sawPassword bool
	for _, e := range pod.Spec.Containers[0].Env {
		if e.Name == "PGPASSWORD" {
			sawPassword = e.ValueFrom != nil && e.Value == ""
		}
	}
	if !sawPassword {
		t.Error("PGPASSWORD must come from a secret reference, never a literal")
	}
}

func TestInstanceRecordsWhatTheProbeSaw(t *testing.T) {
	const msg = `{"server_version":"16.4","databases":"7","data_dir":"/var/lib/postgresql/data",` +
		`"disk_total_bytes":"536870912000","disk_free_bytes":"322122547200"}`
	r, key := newInstTest(t, anInstance(), finishedPod(corev1.PodSucceeded, msg))
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key}); err != nil {
		t.Fatal(err)
	}
	var got doblurav1alpha1.OdooInstance
	if err := r.Get(context.Background(), key, &got); err != nil {
		t.Fatal(err)
	}
	if got.Status.ServerVersion != "16.4" {
		t.Errorf("serverVersion = %q, want 16.4 — this is the field that warns about a client/server mismatch", got.Status.ServerVersion)
	}
	if got.Status.DiskFreeGi == nil || *got.Status.DiskFreeGi != 300 {
		t.Errorf("diskFreeGi = %v, want 300 (322122547200 bytes)", got.Status.DiskFreeGi)
	}
	if got.Status.DiskTotalGi == nil || *got.Status.DiskTotalGi != 500 {
		t.Errorf("diskTotalGi = %v, want 500", got.Status.DiskTotalGi)
	}
	if got.Status.LastProbe == nil {
		t.Error("lastProbe must be set: placement refuses an instance it has never observed")
	}
	if c := findLinkCond(got.Status.Conditions, doblurav1alpha1.ConditionReachable); c == nil ||
		c.Status != metav1.ConditionTrue {
		t.Error("Reachable must be True after a successful probe")
	}
	// The Pod is cleaned up: its env references the admin password Secret.
	var pod corev1.Pod
	if err := r.Get(context.Background(), types.NamespacedName{
		Namespace: "d", Name: probePodName("pg-1")}, &pod); err == nil {
		t.Error("the finished probe Pod should be deleted")
	}
}

func TestInstanceKeepsTheLastObservationWhenTheProbeFails(t *testing.T) {
	// The rule carried over from RunboatLink: "the disk is full" and "we could not
	// measure the disk" are different facts, and reporting the second as the first
	// would make placement refuse a healthy server.
	inst := anInstance()
	old := metav1.NewTime(time.Now().Add(-time.Hour))
	free := int32(300)
	inst.Status = doblurav1alpha1.OdooInstanceStatus{
		ServerVersion: "16.4", DiskFreeGi: &free, LastProbe: &old,
	}
	r, key := newInstTest(t, inst,
		finishedPod(corev1.PodFailed, "psql: could not connect to server: Connection refused"))
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key}); err != nil {
		t.Fatalf("a failed probe must not fail the reconcile: %v", err)
	}
	var got doblurav1alpha1.OdooInstance
	if err := r.Get(context.Background(), key, &got); err != nil {
		t.Fatal(err)
	}
	if got.Status.ServerVersion != "16.4" || got.Status.DiskFreeGi == nil || *got.Status.DiskFreeGi != 300 {
		t.Errorf("the previous observation was discarded: version=%q free=%v",
			got.Status.ServerVersion, got.Status.DiskFreeGi)
	}
	if c := findLinkCond(got.Status.Conditions, doblurav1alpha1.ConditionReachable); c == nil ||
		c.Status != metav1.ConditionFalse {
		t.Error("Reachable must go False when the probe fails")
	}
	if !strings.Contains(got.Status.Message, "Connection refused") {
		t.Errorf("the container's own words should survive: %q", got.Status.Message)
	}
}

func TestInstanceDoesNotReprobeWhileTheObservationIsFresh(t *testing.T) {
	inst := anInstance()
	now := metav1.Now()
	free := int32(300)
	inst.Status = doblurav1alpha1.OdooInstanceStatus{
		ServerVersion: "16.4", DiskFreeGi: &free, LastProbe: &now,
	}
	r, key := newInstTest(t, inst)
	res, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key})
	if err != nil {
		t.Fatal(err)
	}
	var pod corev1.Pod
	if err := r.Get(context.Background(), types.NamespacedName{
		Namespace: "d", Name: probePodName("pg-1")}, &pod); err == nil {
		t.Error("probed again while the observation was still fresh")
	}
	// It should wait out the remainder of the interval, not spin.
	if res.RequeueAfter < 5*time.Minute {
		t.Errorf("requeued in %v; should wait most of the 10m interval", res.RequeueAfter)
	}
}

func TestInstanceCountsPlacedDatabasesFromBothFields(t *testing.T) {
	// spec.instanceRef is what a human asked for; status.placedOn is what the
	// placer chose. Counting only one of them undercounts and lets MaxDatabases be
	// passed.
	db1 := &doblurav1alpha1.OdooDatabase{
		ObjectMeta: metav1.ObjectMeta{Name: "a", Namespace: "d"},
		Spec:       doblurav1alpha1.OdooDatabaseSpec{Name: "a", Role: doblurav1alpha1.RoleQA, InstanceRef: "pg-1"},
	}
	db2 := &doblurav1alpha1.OdooDatabase{
		ObjectMeta: metav1.ObjectMeta{Name: "b", Namespace: "d"},
		Spec:       doblurav1alpha1.OdooDatabaseSpec{Name: "b", Role: doblurav1alpha1.RoleQA},
		Status:     doblurav1alpha1.OdooDatabaseStatus{PlacedOn: "pg-1"},
	}
	elsewhere := &doblurav1alpha1.OdooDatabase{
		ObjectMeta: metav1.ObjectMeta{Name: "c", Namespace: "d"},
		Spec:       doblurav1alpha1.OdooDatabaseSpec{Name: "c", Role: doblurav1alpha1.RoleQA, InstanceRef: "pg-2"},
	}
	r, key := newInstTest(t, anInstance(), db1, db2, elsewhere)
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key}); err != nil {
		t.Fatal(err)
	}
	var got doblurav1alpha1.OdooInstance
	if err := r.Get(context.Background(), key, &got); err != nil {
		t.Fatal(err)
	}
	if got.Status.Databases != 2 {
		t.Errorf("databases = %d, want 2 (one by instanceRef, one by placedOn)", got.Status.Databases)
	}
	if got.Status.Available != 18 {
		t.Errorf("available = %d, want 18", got.Status.Available)
	}
}

func TestInstanceCordonedIsNotSchedulable(t *testing.T) {
	inst := anInstance()
	yes := true
	inst.Spec.Unschedulable = &yes
	now := metav1.Now()
	free := int32(300)
	inst.Status = doblurav1alpha1.OdooInstanceStatus{
		DiskFreeGi: &free, LastProbe: &now,
		Conditions: []metav1.Condition{{
			Type: doblurav1alpha1.ConditionReachable, Status: metav1.ConditionTrue,
			Reason: "Probed", LastTransitionTime: now,
		}},
	}
	r, key := newInstTest(t, inst)
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key}); err != nil {
		t.Fatal(err)
	}
	var got doblurav1alpha1.OdooInstance
	if err := r.Get(context.Background(), key, &got); err != nil {
		t.Fatal(err)
	}
	c := findLinkCond(got.Status.Conditions, doblurav1alpha1.ConditionSchedulable)
	if c == nil || c.Status != metav1.ConditionFalse || c.Reason != "Cordoned" {
		t.Fatalf("a cordoned instance must not be Schedulable: %+v", c)
	}
}

// ─────────────── parsing ───────────────

func TestParseProbeResultRefusesToInventAnObservation(t *testing.T) {
	// An empty or non-JSON message must be an error, never an empty observation:
	// writing zeroes would look observed to placement, and a zeroed disk reads as
	// full.
	for name, msg := range map[string]string{
		"empty":       "",
		"whitespace":  "  \n ",
		"log fallout": "psql: error: connection to server failed",
		"broken json": `{"server_version":`,
	} {
		if _, err := parseProbeResult(msg); err == nil {
			t.Errorf("%s: accepted %q as an observation", name, msg)
		}
	}
}

func TestParseProbeResultReportsAPartialAnswer(t *testing.T) {
	// Connected, but could not measure free space — the usual cause is that a
	// CREATEDB-only user cannot read the filesystem. That must be reported, not
	// hidden, because somebody has to know before trusting reservedGi.
	const msg = `{"server_version":"16.4","databases":"3","data_dir":"/x",` +
		`"disk_total_bytes":"","disk_free_bytes":"","error":"the probe cannot see the data directory"}`
	res, err := parseProbeResult(msg)
	if err != nil {
		t.Fatalf("a partial answer is still an answer: %v", err)
	}
	if res.ServerVersion != "16.4" || res.Error == "" {
		t.Errorf("both the version and the explanation must survive: %+v", res)
	}
	if gib(res.DiskFreeBytes) != nil {
		t.Error("an unmeasured disk must stay nil, not become 0")
	}
}

func TestGibRoundsDown(t *testing.T) {
	// Rounding down is the safe direction on free space: the opposite would let
	// capacity.reservedGi overshoot by up to a gibibyte.
	for in, want := range map[string]int32{
		"1073741824": 1, // exactly 1 GiB
		"2147483647": 1, // one byte short of 2 GiB
		"0":          0,
	} {
		got := gib(in)
		if got == nil || *got != want {
			t.Errorf("gib(%s) = %v, want %d", in, got, want)
		}
	}
	for _, bad := range []string{"", "   ", "not a number", "-5"} {
		if gib(bad) != nil {
			t.Errorf("gib(%q) should be nil", bad)
		}
	}
}

func TestObserveEveryHasADefaultAndAFloor(t *testing.T) {
	var s doblurav1alpha1.OdooInstanceSpec
	if got := s.ObserveEvery().Duration; got != 10*time.Minute {
		t.Errorf("default = %v, want 10m", got)
	}
	s.ObserveInterval = &metav1.Duration{Duration: time.Millisecond}
	if got := s.ObserveEvery().Duration; got != time.Minute {
		t.Errorf("1ms became %v, want the 1m floor — each probe is a Pod", got)
	}
}
