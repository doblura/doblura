// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Antoni Romera

package webhook

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	admissionv1 "k8s.io/api/admission/v1"
	authenticationv1 "k8s.io/api/authentication/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	doblurav1alpha1 "github.com/doblura/doblura/api/v1alpha1"
)

const (
	operatorSA  = "system:serviceaccount:doblura-system:doblura"
	support     = "ana@example.com"
	otherPerson = "bruno@example.com"
)

func testScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := doblurav1alpha1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	return s
}

// env builds an environment the way the API server would hand it to the webhook:
// defaults applied, and the creator annotation already stamped by the mutating
// half.
func env(ns, name, tenant, creator string, mods ...func(*doblurav1alpha1.OdooEnvironment)) *doblurav1alpha1.OdooEnvironment {
	e := &doblurav1alpha1.OdooEnvironment{
		ObjectMeta: metav1.ObjectMeta{
			Name:        name,
			Namespace:   ns,
			Annotations: map[string]string{CreatorAnnotation: creator},
		},
		Spec: doblurav1alpha1.OdooEnvironmentSpec{
			Image:     "odoo:19.0",
			ForTenant: tenant,
			Data:      doblurav1alpha1.EnvData{Type: doblurav1alpha1.DataDemo},
			Lifecycle: doblurav1alpha1.EnvLifecycle{Type: doblurav1alpha1.LifecycleEphemeral},
			Database:  doblurav1alpha1.DatabaseSpec{Host: "pg", User: "odoo", PasswordSecret: "pg"},
		},
	}
	for _, m := range mods {
		m(e)
	}
	return e
}

func tenant(ns, name string, max *int32) *doblurav1alpha1.OdooTenant {
	return &doblurav1alpha1.OdooTenant{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: doblurav1alpha1.OdooTenantSpec{
			DisplayName:              strings.ToUpper(name),
			MaxEphemeralEnvironments: max,
		},
	}
}

func ptr32(i int32) *int32 { return &i }

// request wraps an object the way the API server does, with the authenticated
// identity in UserInfo — the field no client can write to.
func request(t *testing.T, user string, e *doblurav1alpha1.OdooEnvironment) admission.Request {
	t.Helper()
	raw, err := json.Marshal(e)
	if err != nil {
		t.Fatal(err)
	}
	return admission.Request{AdmissionRequest: admissionv1.AdmissionRequest{
		Operation: admissionv1.Create,
		Namespace: e.Namespace,
		Name:      e.Name,
		UserInfo:  authenticationv1.UserInfo{Username: user},
		Object:    runtime.RawExtension{Raw: raw},
	}}
}

func quota(t *testing.T, perCreator int32, objs ...client.Object) *EnvironmentQuota {
	t.Helper()
	s := testScheme(t)
	return &EnvironmentQuota{
		Reader:        fake.NewClientBuilder().WithScheme(s).WithObjects(objs...).Build(),
		Decoder:       admission.NewDecoder(s),
		ExemptUsers:   map[string]bool{operatorSA: true},
		MaxPerCreator: perCreator,
	}
}

// The reason this change exists: at the limit, the create is refused.
func TestAtTheTenantLimitTheCreateIsRefused(t *testing.T) {
	q := quota(t, 50,
		tenant("demo", "acme", ptr32(3)),
		env("demo", "one", "acme", support),
		env("demo", "two", "acme", otherPerson),
		env("demo", "three", "acme", otherPerson),
	)

	resp := q.Handle(context.Background(), request(t, support, env("demo", "four", "acme", support)))
	if resp.Allowed {
		t.Fatal("the fourth environment for a customer capped at three must be refused")
	}

	msg := resp.Result.Message
	// A refusal has to name the limit, the count, and what to do next. Each of
	// these has been missing from somebody's error message at some point.
	for _, want := range []string{
		"acme",                           // which customer
		"3 of 3 open",                    // the count and the limit
		"1 of them yours",                // how much of it is the caller's
		"demo/one",                       // which environments
		"kubectl delete odooenvironment", // the way out
		"doblura-platform",               // who to ask
		"maxEphemeralEnvironments",       // and what they have to change
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("the refusal must mention %q; got: %s", want, msg)
		}
	}
}

// The check that proves the webhook is not simply refusing everything, which is
// what a broken one looks like from the rejection side.
func TestUnderTheLimitTheCreateIsAdmitted(t *testing.T) {
	q := quota(t, 50,
		tenant("demo", "acme", ptr32(3)),
		env("demo", "one", "acme", support),
		env("demo", "two", "acme", otherPerson),
	)

	resp := q.Handle(context.Background(), request(t, support, env("demo", "three", "acme", support)))
	if !resp.Allowed {
		t.Fatalf("the third environment of three must be admitted; got: %s", resp.Result.Message)
	}
}

// A tenant that never mentioned a limit gets the CRD's default, and the fallback in
// Go has to be the same number the API server would have applied.
func TestATenantWithNoExplicitLimitTakesTheDefault(t *testing.T) {
	if doblurav1alpha1.DefaultMaxEphemeralEnvironments != 3 {
		t.Fatalf("this test is written around a default of 3; it is %d",
			doblurav1alpha1.DefaultMaxEphemeralEnvironments)
	}

	existing := []client.Object{tenant("demo", "acme", nil)}
	for _, n := range []string{"one", "two"} {
		existing = append(existing, env("demo", n, "acme", otherPerson))
	}

	// Two open, no explicit limit: the third is fine.
	q := quota(t, 50, existing...)
	if resp := q.Handle(context.Background(), request(t, support, env("demo", "three", "acme", support))); !resp.Allowed {
		t.Fatalf("two of the default three must leave room; got: %s", resp.Result.Message)
	}

	// Three open: the fourth is not.
	existing = append(existing, env("demo", "three", "acme", otherPerson))
	q = quota(t, 50, existing...)
	resp := q.Handle(context.Background(), request(t, support, env("demo", "four", "acme", support)))
	if resp.Allowed {
		t.Fatal("a tenant with no explicit limit must still be capped at the default of 3")
	}
	if !strings.Contains(resp.Result.Message, "3 of 3 open") {
		t.Errorf("the refusal should quote the defaulted limit; got: %s", resp.Result.Message)
	}
}

// Zero is a decision — "nobody may copy this customer's data any more" — and the
// pointer on the field is what makes it expressible at all.
func TestATenantLimitOfZeroRefusesTheFirstEnvironment(t *testing.T) {
	q := quota(t, 50, tenant("demo", "acme", ptr32(0)))

	resp := q.Handle(context.Background(), request(t, support, env("demo", "one", "acme", support)))
	if resp.Allowed {
		t.Fatal("a limit of zero must refuse the first environment, not be read as unset")
	}
	if !strings.Contains(resp.Result.Message, "zero ephemeral environments") {
		t.Errorf("a zero quota deserves its own message rather than 'delete one to make room'; got: %s",
			resp.Result.Message)
	}
}

// The operator creates environments on the cluster's behalf. A human's allowance
// must never throttle it.
func TestTheOperatorsServiceAccountIsExempt(t *testing.T) {
	objs := []client.Object{tenant("demo", "acme", ptr32(1)), env("demo", "one", "acme", support)}
	q := quota(t, 1, objs...)

	// Same request, two identities: refused for the person, admitted for the
	// operator. Asserting both is what makes this a test of the exemption rather
	// than of the quota being off.
	if resp := q.Handle(context.Background(), request(t, support, env("demo", "two", "acme", support))); resp.Allowed {
		t.Fatal("the person is over both limits and must be refused")
	}
	resp := q.Handle(context.Background(), request(t, operatorSA, env("demo", "two", "acme", operatorSA)))
	if !resp.Allowed {
		t.Fatalf("the operator's own ServiceAccount must not be subject to the quota; got: %s",
			resp.Result.Message)
	}
}

// The hole the per-tenant limit alone does not close: many customers, one person.
func TestThePerPersonLimitCountsAcrossTenantsAndNamespaces(t *testing.T) {
	objs := []client.Object{
		tenant("demo", "acme", ptr32(3)),
		tenant("demo", "globex", ptr32(3)),
		tenant("other", "initech", ptr32(3)),
		env("demo", "a", "acme", support),
		env("demo", "b", "globex", support),
		env("other", "c", "initech", support),
		// Not the caller's, and in the mix on purpose: a count that included
		// these would refuse the wrong person.
		env("demo", "d", "acme", otherPerson),
		env("demo", "e", "globex", otherPerson),
	}

	q := quota(t, 3, objs...)
	resp := q.Handle(context.Background(), request(t, support, env("demo", "f", "", support)))
	if resp.Allowed {
		t.Fatal("three environments across three customers must exhaust an allowance of three")
	}
	for _, want := range []string{"3 of your 3", "other/c", "maxEnvironmentsPerCreator"} {
		if !strings.Contains(resp.Result.Message, want) {
			t.Errorf("the refusal must mention %q; got: %s", want, resp.Result.Message)
		}
	}

	// And the other person, with two, is unaffected.
	if resp := q.Handle(context.Background(), request(t, otherPerson, env("demo", "g", "", otherPerson))); !resp.Allowed {
		t.Fatalf("somebody else's environments must not spend this person's allowance; got: %s",
			resp.Result.Message)
	}
}

// An environment with no customer declared is still counted against the person.
// Otherwise the quota is opt-out: omit forTenant and open fifty.
func TestAnEnvironmentWithNoTenantStillSpendsThePersonalAllowance(t *testing.T) {
	q := quota(t, 2,
		env("demo", "a", "", support),
		env("demo", "b", "", support),
	)
	resp := q.Handle(context.Background(), request(t, support, env("demo", "c", "", support)))
	if resp.Allowed {
		t.Fatal("leaving forTenant empty must not be a way past the quota")
	}
}

func TestWhatIsAndIsNotCounted(t *testing.T) {
	deleted := metav1.NewTime(metav1.Now().Time)
	cases := []struct {
		name    string
		open    func(*doblurav1alpha1.OdooEnvironment)
		counted bool
	}{
		{
			name:    "a failed environment counts: it still owns a database",
			open:    func(e *doblurav1alpha1.OdooEnvironment) { e.Status.Phase = doblurav1alpha1.EnvFailed },
			counted: true,
		},
		{
			name: "a hibernated environment counts: it wakes up on the next request",
			open: func(e *doblurav1alpha1.OdooEnvironment) {
				e.Status.Phase = doblurav1alpha1.EnvHibernated
			},
			counted: true,
		},
		{
			name:    "an expired one does not: the controller has already asked for its deletion",
			open:    func(e *doblurav1alpha1.OdooEnvironment) { e.Status.Phase = doblurav1alpha1.EnvExpired },
			counted: false,
		},
		{
			name: "one being deleted does not: the finalizer is dropping its database now",
			open: func(e *doblurav1alpha1.OdooEnvironment) {
				e.DeletionTimestamp = &deleted
				e.Finalizers = []string{"doblura.dev/environment-cleanup"}
			},
			counted: false,
		},
		{
			name: "a persistent one does not: it is somebody's staging, not a throwaway",
			open: func(e *doblurav1alpha1.OdooEnvironment) {
				e.Spec.Lifecycle.Type = doblurav1alpha1.LifecyclePersistent
			},
			counted: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// One existing environment against a limit of one: whether the create
			// is refused is exactly the question of whether it was counted.
			q := quota(t, 50, tenant("demo", "acme", ptr32(1)), env("demo", "one", "acme", support, tc.open))
			resp := q.Handle(context.Background(), request(t, support, env("demo", "two", "acme", support)))
			if resp.Allowed == tc.counted {
				t.Errorf("counted=%v but the create was allowed=%v: %s", tc.counted, resp.Allowed, resp.Result.Message)
			}
		})
	}
}

// A Persistent environment is not subject to the quota at all, which is a separate
// thing from not being counted by it.
func TestAPersistentEnvironmentIsNotSubjectToTheQuota(t *testing.T) {
	q := quota(t, 1, tenant("demo", "acme", ptr32(1)), env("demo", "one", "acme", support))
	resp := q.Handle(context.Background(), request(t, support,
		env("demo", "staging", "acme", support, func(e *doblurav1alpha1.OdooEnvironment) {
			e.Spec.Lifecycle.Type = doblurav1alpha1.LifecyclePersistent
		})))
	if !resp.Allowed {
		t.Fatalf("a Persistent environment must not be capped by a throwaway quota; got: %s", resp.Result.Message)
	}
}

// The same decision the handover check makes: Doblura stays usable before the
// catalogue is filled in, and says what it could not check.
func TestAnUnknownTenantLeavesOnlyThePersonalLimit(t *testing.T) {
	q := quota(t, 2, env("demo", "one", "nowhere", support))

	if resp := q.Handle(context.Background(), request(t, support, env("demo", "two", "nowhere", support))); !resp.Allowed {
		t.Fatalf("an environment for a customer with no record must not be refused; got: %s", resp.Result.Message)
	}
	// But the personal allowance still bounds it, which is why the missing
	// record is not a way through.
	q = quota(t, 2, env("demo", "one", "nowhere", support), env("demo", "two", "nowhere", support))
	if resp := q.Handle(context.Background(), request(t, support, env("demo", "three", "nowhere", support))); resp.Allowed {
		t.Fatal("a customer with no record must still be bounded by the personal allowance")
	}
}

// ─────────────── the creator annotation ───────────────

// The webhook writes the creator itself. What the client sent is overwritten, not
// merged, not honoured.
func TestTheCreatorAnnotationIsStampedByTheServer(t *testing.T) {
	s := testScheme(t)
	m := &EnvironmentCreator{Decoder: admission.NewDecoder(s)}

	forged := env("demo", "one", "acme", "somebody-else@example.com")
	resp := m.Handle(context.Background(), request(t, support, forged))
	if !resp.Allowed {
		t.Fatalf("the stamping webhook must not refuse anything: %s", resp.Result.Message)
	}
	if len(resp.Patches) != 1 {
		t.Fatalf("expected exactly one patch operation, got %d: %+v", len(resp.Patches), resp.Patches)
	}
	p := resp.Patches[0]
	// The `/` in the annotation key is the JSON Pointer separator and has to be
	// escaped, or the patch silently addresses a nested object nobody has.
	if p.Path != "/metadata/annotations/doblura.dev~1created-by" {
		t.Errorf("wrong patch path: %s", p.Path)
	}
	if p.Value != support {
		t.Errorf("the stamp must be the authenticated identity, not the client's word: %v", p.Value)
	}
}

// An object with no annotations at all: RFC 6902 will not add a member to an
// object that does not exist, so the whole map has to be created in one operation.
func TestTheCreatorAnnotationIsStampedOnAnObjectWithNoAnnotations(t *testing.T) {
	s := testScheme(t)
	m := &EnvironmentCreator{Decoder: admission.NewDecoder(s)}

	bare := env("demo", "one", "acme", "")
	bare.Annotations = nil
	resp := m.Handle(context.Background(), request(t, support, bare))
	if len(resp.Patches) != 1 || resp.Patches[0].Path != "/metadata/annotations" {
		t.Fatalf("expected the annotations map to be created in one operation: %+v", resp.Patches)
	}
	got, ok := resp.Patches[0].Value.(map[string]string)
	if !ok || got[CreatorAnnotation] != support {
		t.Fatalf("the created map must carry the authenticated identity: %+v", resp.Patches[0].Value)
	}
}

// And the validating half does not trust the annotation either: it checks that the
// stamp matches the caller. This is what makes the count arithmetic on server data
// rather than on a field the caller chose.
func TestAForgedCreatorAnnotationIsRefused(t *testing.T) {
	q := quota(t, 50, tenant("demo", "acme", ptr32(3)))

	forged := env("demo", "one", "acme", "somebody-else@example.com")
	resp := q.Handle(context.Background(), request(t, support, forged))
	if resp.Allowed {
		t.Fatal("an object naming a creator other than the caller must be refused")
	}
	for _, want := range []string{"doblura-creator", "somebody-else@example.com", support} {
		if !strings.Contains(resp.Result.Message, want) {
			t.Errorf("the refusal must mention %q; got: %s", want, resp.Result.Message)
		}
	}
}

// A built image's addons directory reaches the environment without anybody
// typing it.
//
// This is the loop OdooBuild opens: the image carries its modules in a directory
// the flavour list knows nothing about, and spec.addons.baked is the field that
// puts it on the addons path. Left to a person, that is a path transcribed by
// hand into the one field whose failure mode is Odoo starting happily and then
// not having the module.
//
// From the STUDY, which observed the directory by running the image. Never from a
// convention: an addons_path entry that does not exist is the failure envpod
// warns about, and a guess is how you get one.
func TestABuiltImagesAddonsReachTheEnvironment(t *testing.T) {
	tenant := &doblurav1alpha1.OdooTenant{
		Status: doblurav1alpha1.OdooTenantStatus{
			ImageStudies: []doblurav1alpha1.ImageStudy{{
				Name: "erp", Image: "r/x:1",
				AddonsPaths: []string{
					"/opt/doblura/addons",
					// Odoo's own, which is already found and only makes the field
					// harder to read.
					"/usr/lib/python3/dist-packages/odoo/addons",
				},
			}},
		},
	}
	entry := &doblurav1alpha1.ImageCatalogueEntry{Name: "erp", Image: "r/x:1"}
	env := &doblurav1alpha1.OdooEnvironment{}

	got := studiedAddons(tenant, entry, env)
	if len(got) != 1 || got[0] != "/opt/doblura/addons" {
		t.Fatalf("the built image's addons did not reach the environment: %v", got)
	}
	// Odoo's own package path is deliberately NOT carried into the field. Writing
	// an addons_path does not lose it — measured, with a config naming only
	// /opt/doblura/addons, and Odoo still reported dist-packages and
	// data_dir/addons/18.0 alongside it. Putting it in the spec would record a
	// decision doblura did not make.
	for _, p := range got {
		if strings.Contains(p, "/dist-packages/") {
			t.Errorf("Odoo's own path was written into the spec: %v", got)
		}
	}

	// Already declared: nothing to add, and no patch, because a no-op patch still
	// rewrites the field and shows up as a change in every audit of the object.
	env.Spec.Addons.Baked = []string{"/opt/doblura/addons"}
	if got := studiedAddons(tenant, entry, env); got != nil {
		t.Errorf("an environment that needs no patch got one: %v", got)
	}

	// An image whose only studied path is Odoo's own gets NO patch: the official
	// image finds its own addons, and writing the field for it would replace a
	// default that was already right.
	plain := &doblurav1alpha1.OdooTenant{
		Status: doblurav1alpha1.OdooTenantStatus{
			ImageStudies: []doblurav1alpha1.ImageStudy{{
				Name: "erp", Image: "r/x:1",
				AddonsPaths: []string{"/usr/lib/python3/dist-packages/odoo/addons"},
			}},
		},
	}
	if got := studiedAddons(plain, entry, &doblurav1alpha1.OdooEnvironment{}); got != nil {
		t.Errorf("an official image was given an addons_path it does not need: %v", got)
	}

	// A study of a DIFFERENT image is not a study of this one. The entry can be
	// repointed, and a stale report read as current is worse than no report.
	entry2 := &doblurav1alpha1.ImageCatalogueEntry{Name: "erp", Image: "r/x:2"}
	if got := studiedAddons(tenant, entry2, &doblurav1alpha1.OdooEnvironment{}); got != nil {
		t.Errorf("a study of another image was applied: %v", got)
	}
}
