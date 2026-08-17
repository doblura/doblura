// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Antoni Romera

package controller

import (
	"context"
	"errors"
	"fmt"
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

// ─────────────── The fake Runboat ───────────────

type fakeRunboat struct {
	builds  []RunboatRemoteBuild
	pollErr error

	// acted records every call, in order. The test that matters here asserts on
	// its LENGTH: a Reset executed twice is a database wiped twice.
	acted  []string
	actErr error
	nPolls int
	nActs  int
}

func (f *fakeRunboat) Builds(_ context.Context, _ *doblurav1alpha1.RunboatLinkSpec, _ *RunboatCredentials) ([]RunboatRemoteBuild, error) {
	f.nPolls++
	if f.pollErr != nil {
		return nil, f.pollErr
	}
	return f.builds, nil
}

func (f *fakeRunboat) Act(_ context.Context, _ *doblurav1alpha1.RunboatLinkSpec, _ *RunboatCredentials, build string, action doblurav1alpha1.RunboatAction) error {
	f.nActs++
	f.acted = append(f.acted, fmt.Sprintf("%s:%s", action, build))
	return f.actErr
}

func remoteBuild(name, repo, branch string, pr int32, created string) RunboatRemoteBuild {
	b := RunboatRemoteBuild{
		Name:       name,
		DeployLink: "https://" + name + ".example.com",
		WebuiLink:  "https://runboat.example.com/webui/build/" + name,
		Status:     "started",
		Created:    created,
	}
	b.CommitInfo.Repo = repo
	b.CommitInfo.TargetBranch = branch
	b.CommitInfo.GitCommit = "0123456789abcdef"
	if pr > 0 {
		p := pr
		b.CommitInfo.PR = &p
	}
	return b
}

func runboatScheme(t *testing.T) *runtime.Scheme {
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

// newLinkTest wires a reconciler over a fake API server holding one link.
func newLinkTest(t *testing.T, link *doblurav1alpha1.RunboatLink, api runboatAPI, objs ...runtime.Object) (*RunboatLinkReconciler, types.NamespacedName) {
	t.Helper()
	s := runboatScheme(t)
	all := append([]runtime.Object{link}, objs...)
	c := fake.NewClientBuilder().
		WithScheme(s).
		WithRuntimeObjects(all...).
		WithStatusSubresource(&doblurav1alpha1.RunboatLink{}).
		Build()
	return &RunboatLinkReconciler{Client: c, Scheme: s, API: api},
		types.NamespacedName{Namespace: link.Namespace, Name: link.Name}
}

func baseLink() *doblurav1alpha1.RunboatLink {
	return &doblurav1alpha1.RunboatLink{
		ObjectMeta: metav1.ObjectMeta{Name: "oca", Namespace: "doblura-system"},
		Spec: doblurav1alpha1.RunboatLinkSpec{
			URL:       "https://runboat.example.com",
			MaxBuilds: 200,
		},
	}
}

// ─────────────── Mirroring ───────────────

func TestRunboatMirrorsBuildsNewestFirst(t *testing.T) {
	api := &fakeRunboat{builds: []RunboatRemoteBuild{
		remoteBuild("old", "oca/account-financial-tools", "17.0", 11, "2026-01-01T10:00:00+00:00"),
		remoteBuild("new", "oca/account-financial-tools", "17.0", 22, "2026-06-01T10:00:00+00:00"),
		remoteBuild("mid", "oca/account-financial-tools", "17.0", 33, "2026-03-01T10:00:00+00:00"),
	}}
	r, key := newLinkTest(t, baseLink(), api)

	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key}); err != nil {
		t.Fatal(err)
	}

	var got doblurav1alpha1.RunboatLink
	if err := r.Get(context.Background(), key, &got); err != nil {
		t.Fatal(err)
	}
	if got.Status.Total != 3 {
		t.Fatalf("total = %d, want 3", got.Status.Total)
	}
	want := []string{"new", "mid", "old"}
	for i, w := range want {
		if got.Status.Builds[i].Name != w {
			t.Errorf("builds[%d] = %q, want %q", i, got.Status.Builds[i].Name, w)
		}
	}
	if got.Status.Truncated {
		t.Error("truncated should be false for 3 builds under a cap of 200")
	}
	if got.Status.LastPoll == nil {
		t.Error("lastPoll must be set after a successful poll")
	}
	// The mirror carries the links, because being able to open the build is the
	// entire point of showing it here.
	if got.Status.Builds[0].DeployLink == "" || got.Status.Builds[0].WebuiLink == "" {
		t.Error("deployLink and webuiLink must be mirrored")
	}
	if pr := got.Status.Builds[0].PR; pr == nil || *pr != 22 {
		t.Errorf("PR not mirrored: %v", pr)
	}
}

func TestRunboatTruncationIsRecordedNotSilent(t *testing.T) {
	var builds []RunboatRemoteBuild
	for i := 0; i < 10; i++ {
		builds = append(builds, remoteBuild(
			fmt.Sprintf("b%02d", i), "oca/x", "17.0", int32(i+1),
			fmt.Sprintf("2026-01-%02dT10:00:00+00:00", i+1)))
	}
	link := baseLink()
	link.Spec.MaxBuilds = 4
	api := &fakeRunboat{builds: builds}
	r, key := newLinkTest(t, link, api)

	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key}); err != nil {
		t.Fatal(err)
	}
	var got doblurav1alpha1.RunboatLink
	if err := r.Get(context.Background(), key, &got); err != nil {
		t.Fatal(err)
	}

	if len(got.Status.Builds) != 4 {
		t.Fatalf("kept %d builds, want 4", len(got.Status.Builds))
	}
	if got.Status.Total != 10 {
		t.Errorf("total = %d, want 10: the total must report everything that matched", got.Status.Total)
	}
	if !got.Status.Truncated {
		t.Error("truncated must be true: a capped list reads exactly like a complete one")
	}
	if !strings.Contains(got.Status.Message, "capped") {
		t.Errorf("the message must say it was capped, got %q", got.Status.Message)
	}
}

func TestRunboatFilterIsCaseInsensitive(t *testing.T) {
	// Runboat lowercases repo names on the way in. A filter written the way it
	// appears on GitHub must still match, or the link looks broken.
	api := &fakeRunboat{builds: []RunboatRemoteBuild{
		remoteBuild("a", "oca/account-financial-tools", "17.0", 1, "2026-01-01T10:00:00Z"),
		remoteBuild("b", "tecnativa/something", "17.0", 2, "2026-01-02T10:00:00Z"),
	}}
	link := baseLink()
	link.Spec.Filter = &doblurav1alpha1.RunboatFilter{
		Repos: []string{"OCA/account-financial-tools"},
	}
	r, key := newLinkTest(t, link, api)

	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key}); err != nil {
		t.Fatal(err)
	}
	var got doblurav1alpha1.RunboatLink
	if err := r.Get(context.Background(), key, &got); err != nil {
		t.Fatal(err)
	}
	if got.Status.Total != 1 || got.Status.Builds[0].Name != "a" {
		t.Fatalf("filter did not match the mixed-case repo: %d builds %+v",
			got.Status.Total, buildNames(got.Status.Builds))
	}
}

func TestRunboatKeepsMirrorWhenPollFails(t *testing.T) {
	// The distinction the whole design rests on: "every build disappeared" and
	// "Doblura cannot see them" are different facts.
	api := &fakeRunboat{builds: []RunboatRemoteBuild{
		remoteBuild("a", "oca/x", "17.0", 1, "2026-01-01T10:00:00Z"),
	}}
	r, key := newLinkTest(t, baseLink(), api)

	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key}); err != nil {
		t.Fatal(err)
	}

	api.pollErr = errors.New("cannot reach runboat: connection refused")
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key}); err != nil {
		t.Fatalf("a failed poll must not fail the reconcile: %v", err)
	}

	var got doblurav1alpha1.RunboatLink
	if err := r.Get(context.Background(), key, &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Status.Builds) != 1 {
		t.Errorf("the mirror was cleared on a failed poll: %d builds", len(got.Status.Builds))
	}
	if c := findLinkCond(got.Status.Conditions, doblurav1alpha1.ConditionRunboatReachable); c == nil || c.Status != metav1.ConditionFalse {
		t.Error("Reachable must go False when the poll fails")
	}
}

func TestRunboatStaleMirrorIsFlagged(t *testing.T) {
	link := baseLink()
	link.Spec.PollInterval = &metav1.Duration{Duration: 30 * time.Second}
	// A poll from long ago, and one that keeps failing: the builds stay, but the
	// interface has to be told they cannot be trusted.
	old := metav1.NewTime(time.Now().Add(-2 * time.Hour))
	link.Status = doblurav1alpha1.RunboatLinkStatus{
		LastPoll: &old,
		Builds:   []doblurav1alpha1.RunboatBuild{{Name: "ghost"}},
	}
	api := &fakeRunboat{pollErr: errors.New("timeout")}
	r, key := newLinkTest(t, link, api)

	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key}); err != nil {
		t.Fatal(err)
	}
	var got doblurav1alpha1.RunboatLink
	if err := r.Get(context.Background(), key, &got); err != nil {
		t.Fatal(err)
	}
	c := findLinkCond(got.Status.Conditions, doblurav1alpha1.ConditionMirrorFresh)
	if c == nil || c.Status != metav1.ConditionFalse {
		t.Fatalf("MirrorFresh must be False after 2h with a 30s interval, got %+v", c)
	}
	if len(got.Status.Builds) != 1 {
		t.Error("a stale mirror still shows what it last knew")
	}
}

// ─────────────── Actions ───────────────

func TestRunboatActionRunsOnceEvenAcrossManyReconciles(t *testing.T) {
	// The one that matters. Reset redeploys, which reinitializes the database.
	// A reconcile happens for any reason at any time; without the idempotency key
	// every tick would wipe the build again.
	api := &fakeRunboat{builds: []RunboatRemoteBuild{
		remoteBuild("pr-42", "oca/x", "17.0", 42, "2026-01-01T10:00:00Z"),
	}}
	link := baseLink()
	link.Spec.Auth = &doblurav1alpha1.RunboatAuth{BasicAuthSecret: "runboat-api"}
	link.Spec.AllowedActions = []doblurav1alpha1.RunboatAction{doblurav1alpha1.RunboatReset}
	link.Spec.ActionRequests = []doblurav1alpha1.RunboatActionRequest{{
		ID: "click-abc123", Build: "pr-42",
		Action: doblurav1alpha1.RunboatReset, RequestedBy: "jordi@example.com",
	}}
	sec := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "runboat-api", Namespace: "doblura-system"},
		Data:       map[string][]byte{"username": []byte("admin"), "password": []byte("s3cr3t")},
	}
	r, key := newLinkTest(t, link, api, sec)

	for i := 0; i < 5; i++ {
		if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key}); err != nil {
			t.Fatalf("reconcile %d: %v", i, err)
		}
	}

	if api.nActs != 1 {
		t.Fatalf("the action ran %d times across 5 reconciles, want exactly 1 — %v", api.nActs, api.acted)
	}
	if api.acted[0] != "Reset:pr-42" {
		t.Errorf("acted = %v", api.acted)
	}

	var got doblurav1alpha1.RunboatLink
	if err := r.Get(context.Background(), key, &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Status.ExecutedActions) != 1 {
		t.Fatalf("executedActions = %d, want 1", len(got.Status.ExecutedActions))
	}
	res := got.Status.ExecutedActions[0]
	if !res.Succeeded || res.ID != "click-abc123" || res.RequestedBy != "jordi@example.com" {
		t.Errorf("the audit entry is wrong: %+v", res)
	}
}

func TestRunboatFailedActionIsNotRetriedByItself(t *testing.T) {
	// A failed Reset must still be remembered. Retrying it without anyone asking
	// again would be the controller deciding to wipe a database twice.
	api := &fakeRunboat{
		builds: []RunboatRemoteBuild{remoteBuild("pr-1", "oca/x", "17.0", 1, "2026-01-01T10:00:00Z")},
		actErr: errors.New("runboat answered 500"),
	}
	link := baseLink()
	link.Spec.Auth = &doblurav1alpha1.RunboatAuth{BasicAuthSecret: "s"}
	link.Spec.AllowedActions = []doblurav1alpha1.RunboatAction{doblurav1alpha1.RunboatReset}
	link.Spec.ActionRequests = []doblurav1alpha1.RunboatActionRequest{
		{ID: "x1", Build: "pr-1", Action: doblurav1alpha1.RunboatReset},
	}
	sec := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "s", Namespace: "doblura-system"},
		Data:       map[string][]byte{"username": []byte("u"), "password": []byte("p")},
	}
	r, key := newLinkTest(t, link, api, sec)

	for i := 0; i < 3; i++ {
		if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key}); err != nil {
			t.Fatal(err)
		}
	}
	if api.nActs != 1 {
		t.Fatalf("a failed action was retried: %d attempts", api.nActs)
	}
	var got doblurav1alpha1.RunboatLink
	if err := r.Get(context.Background(), key, &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Status.ExecutedActions) != 1 || got.Status.ExecutedActions[0].Succeeded {
		t.Fatalf("the failure must be recorded as a failure: %+v", got.Status.ExecutedActions)
	}
	if !strings.Contains(got.Status.ExecutedActions[0].Message, "500") {
		t.Errorf("runboat's own error must survive: %q", got.Status.ExecutedActions[0].Message)
	}
}

func TestRunboatActionDeniedWhenNarrowedAfterTheFact(t *testing.T) {
	// CEL rejects this combination on apply. The check is repeated in the
	// controller because allowedActions can be narrowed AFTER a request was
	// accepted, and the narrowing has to win.
	api := &fakeRunboat{builds: []RunboatRemoteBuild{
		remoteBuild("pr-7", "oca/x", "17.0", 7, "2026-01-01T10:00:00Z"),
	}}
	link := baseLink()
	link.Spec.Auth = &doblurav1alpha1.RunboatAuth{BasicAuthSecret: "s"}
	link.Spec.AllowedActions = []doblurav1alpha1.RunboatAction{doblurav1alpha1.RunboatStop}
	link.Spec.ActionRequests = []doblurav1alpha1.RunboatActionRequest{
		{ID: "r1", Build: "pr-7", Action: doblurav1alpha1.RunboatReset},
	}
	sec := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "s", Namespace: "doblura-system"},
		Data:       map[string][]byte{"username": []byte("u"), "password": []byte("p")},
	}
	r, key := newLinkTest(t, link, api, sec)

	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key}); err != nil {
		t.Fatal(err)
	}
	if api.nActs != 0 {
		t.Fatalf("a disallowed action reached runboat: %v", api.acted)
	}
	var got doblurav1alpha1.RunboatLink
	if err := r.Get(context.Background(), key, &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Status.ExecutedActions) != 1 || got.Status.ExecutedActions[0].Succeeded {
		t.Fatal("the denial must be recorded")
	}
	if !strings.Contains(got.Status.ExecutedActions[0].Message, "allowedActions") {
		t.Errorf("the reason must name allowedActions: %q", got.Status.ExecutedActions[0].Message)
	}
}

func TestRunboatActionOnVanishedBuildFails(t *testing.T) {
	api := &fakeRunboat{builds: []RunboatRemoteBuild{}}
	link := baseLink()
	link.Spec.Auth = &doblurav1alpha1.RunboatAuth{BasicAuthSecret: "s"}
	link.Spec.AllowedActions = []doblurav1alpha1.RunboatAction{doblurav1alpha1.RunboatStart}
	link.Spec.ActionRequests = []doblurav1alpha1.RunboatActionRequest{
		{ID: "g1", Build: "pr-gone", Action: doblurav1alpha1.RunboatStart},
	}
	sec := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "s", Namespace: "doblura-system"},
		Data:       map[string][]byte{"username": []byte("u"), "password": []byte("p")},
	}
	r, key := newLinkTest(t, link, api, sec)

	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key}); err != nil {
		t.Fatal(err)
	}
	if api.nActs != 0 {
		t.Error("no call should be made for a build that is not in the mirror")
	}
	var got doblurav1alpha1.RunboatLink
	if err := r.Get(context.Background(), key, &got); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got.Status.ExecutedActions[0].Message, "undeployed") {
		t.Errorf("the message should suggest why: %q", got.Status.ExecutedActions[0].Message)
	}
}

func TestRunboatMissingSecretKeysIsNamed(t *testing.T) {
	// "authentication failed" against a Secret missing a key is an afternoon
	// looking in the wrong place.
	link := baseLink()
	link.Spec.Auth = &doblurav1alpha1.RunboatAuth{BasicAuthSecret: "half"}
	sec := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "half", Namespace: "doblura-system"},
		Data:       map[string][]byte{"username": []byte("u")},
	}
	api := &fakeRunboat{}
	r, key := newLinkTest(t, link, api, sec)

	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key}); err != nil {
		t.Fatal(err)
	}
	if api.nPolls != 0 {
		t.Error("no poll should happen without usable credentials")
	}
	var got doblurav1alpha1.RunboatLink
	if err := r.Get(context.Background(), key, &got); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got.Status.Message, "password") {
		t.Errorf("the message must name the missing key: %q", got.Status.Message)
	}
}

func TestRunboatReadOnlyByDefault(t *testing.T) {
	// A link created without thinking about it is a window, not a remote control.
	s := doblurav1alpha1.RunboatLinkSpec{URL: "https://r.example.com"}
	for _, a := range []doblurav1alpha1.RunboatAction{
		doblurav1alpha1.RunboatStart, doblurav1alpha1.RunboatStop, doblurav1alpha1.RunboatReset,
	} {
		if s.Allows(a) {
			t.Errorf("%s allowed with an empty allowedActions", a)
		}
	}
}

func TestRunboatNoWriteWhenNothingChanged(t *testing.T) {
	// This controller runs on a timer, so without the DeepEqual guard every tick
	// would be an etcd write and every write would wake the watch.
	api := &fakeRunboat{builds: []RunboatRemoteBuild{
		remoteBuild("a", "oca/x", "17.0", 1, "2026-01-01T10:00:00Z"),
	}}
	r, key := newLinkTest(t, baseLink(), api)
	ctx := context.Background()

	if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: key}); err != nil {
		t.Fatal(err)
	}
	var first doblurav1alpha1.RunboatLink
	if err := r.Get(ctx, key, &first); err != nil {
		t.Fatal(err)
	}

	// LastPoll moves on every poll, so the object is expected to change here.
	// What must NOT change is the mirrored content, which is what the interface
	// renders.
	if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: key}); err != nil {
		t.Fatal(err)
	}
	var second doblurav1alpha1.RunboatLink
	if err := r.Get(ctx, key, &second); err != nil {
		t.Fatal(err)
	}
	if len(first.Status.Builds) != len(second.Status.Builds) ||
		first.Status.Builds[0].Name != second.Status.Builds[0].Name {
		t.Error("the mirrored builds changed between two identical polls")
	}
}

// ─────────────── Pure helpers ───────────────

func TestRunboatAPIPathTrimsTrailingSlashes(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"https://r.example.com", "https://r.example.com/api/v1"},
		{"https://r.example.com/", "https://r.example.com/api/v1"},
		{"https://r.example.com///", "https://r.example.com/api/v1"},
	} {
		s := doblurav1alpha1.RunboatLinkSpec{URL: tc.in}
		if got := s.APIPath(); got != tc.want {
			t.Errorf("APIPath(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestRunboatPollIntervalHasAFloor(t *testing.T) {
	// One link must not be able to hammer a shared Runboat.
	s := doblurav1alpha1.RunboatLinkSpec{PollInterval: &metav1.Duration{Duration: time.Second}}
	if got := s.PollEvery().Duration; got != 10*time.Second {
		t.Errorf("a 1s interval became %v, want the 10s floor", got)
	}
	s = doblurav1alpha1.RunboatLinkSpec{}
	if got := s.PollEvery().Duration; got != 60*time.Second {
		t.Errorf("the default became %v, want 60s", got)
	}
}

func TestParseRunboatTimeAcceptsNaiveDatetimes(t *testing.T) {
	// FastAPI emits datetime.datetime, and whether it carries an offset depends on
	// how it was constructed. Parsing only RFC 3339 would silently drop every
	// timestamp from a Runboat storing naive values.
	for _, in := range []string{
		"2026-06-01T10:00:00Z",
		"2026-06-01T10:00:00+02:00",
		"2026-06-01T10:00:00.123456",
		"2026-06-01T10:00:00",
	} {
		if got := parseRunboatTime(in); got == nil {
			t.Errorf("parseRunboatTime(%q) = nil, want a time", in)
		}
	}
	if parseRunboatTime("") != nil {
		t.Error("an empty string must give nil, not the zero time")
	}
	if parseRunboatTime("not a date") != nil {
		t.Error("garbage must give nil rather than a bogus timestamp")
	}
}

func TestRunboatUnknownStatusIsPassedThrough(t *testing.T) {
	// A Runboat that adds a state should show that state, not an empty column.
	b := remoteBuild("x", "oca/x", "17.0", 1, "2026-01-01T10:00:00Z")
	b.Status = "hibernating"
	if got := toMirror(b).Status; string(got) != "hibernating" {
		t.Errorf("status = %q, want it passed through unmapped", got)
	}
}

func TestRunboatHTTPErrorsCarryAHint(t *testing.T) {
	for code, want := range map[int]string{
		401: "basicAuthSecret",
		404: "/api/v1",
		503: "unhealthy",
	} {
		err := runboatHTTPError(code, []byte("nope"))
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error for %d = %q, should mention %q", code, err, want)
		}
	}
}

func TestAppendExecutedStaysWithinTheSchemaBound(t *testing.T) {
	// status.executedActions is MaxItems=64 in the CRD, and a status that exceeds
	// its own schema is rejected on write — failing the whole reconcile over a log
	// entry.
	var done []doblurav1alpha1.RunboatActionResult
	for i := 0; i < 200; i++ {
		done = appendExecuted(done, doblurav1alpha1.RunboatActionResult{ID: fmt.Sprintf("id-%d", i)})
	}
	if len(done) != 64 {
		t.Fatalf("len = %d, want the list capped at 64", len(done))
	}
	// The newest must survive; the idempotency memory is what this list is for.
	if done[len(done)-1].ID != "id-199" {
		t.Errorf("the newest entry was dropped: %s", done[len(done)-1].ID)
	}
}

// findLinkCond is separate from findCond, which takes a rehearsal status.
func findLinkCond(cs []metav1.Condition, t string) *metav1.Condition {
	for i := range cs {
		if cs[i].Type == t {
			return &cs[i]
		}
	}
	return nil
}

func buildNames(bs []doblurav1alpha1.RunboatBuild) []string {
	out := make([]string, 0, len(bs))
	for _, b := range bs {
		out = append(out, b.Name)
	}
	return out
}
