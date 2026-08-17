// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Antoni Romera

package webhook

import (
	"bytes"
	"context"
	"testing"

	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
)

func admissionClient(t *testing.T, objs ...client.Object) client.Client {
	t.Helper()
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	return fake.NewClientBuilder().WithScheme(s).WithObjects(objs...).Build()
}

func validatingConfig(webhooks ...string) *admissionregistrationv1.ValidatingWebhookConfiguration {
	cfg := &admissionregistrationv1.ValidatingWebhookConfiguration{
		ObjectMeta: metav1.ObjectMeta{Name: "doblura-quota"},
	}
	for _, n := range webhooks {
		cfg.Webhooks = append(cfg.Webhooks, admissionregistrationv1.ValidatingWebhook{Name: n})
	}
	return cfg
}

// The chart installs the configuration with no caBundle, because at template time
// there is no CA. This is what fills it in.
func TestTheCABundleIsPublishedIntoEveryWebhook(t *testing.T) {
	ca := []byte("-----BEGIN CERTIFICATE-----\nnot really\n-----END CERTIFICATE-----")
	c := admissionClient(t, validatingConfig("quota.odooenvironment.doblura.dev", "second.doblura.dev"))
	r := &CABundleReconciler{Client: c, Name: "doblura-quota", CABundle: ca}

	if _, err := r.Reconcile(context.Background(), reconcileFor("doblura-quota")); err != nil {
		t.Fatal(err)
	}

	var got admissionregistrationv1.ValidatingWebhookConfiguration
	if err := c.Get(context.Background(), client.ObjectKey{Name: "doblura-quota"}, &got); err != nil {
		t.Fatal(err)
	}
	for _, w := range got.Webhooks {
		if !bytes.Equal(w.ClientConfig.CABundle, ca) {
			t.Errorf("webhook %s was left without the CA: the API server cannot verify it", w.Name)
		}
	}
}

// Drift correction is half the reason this is a controller rather than a patch at
// startup: a `helm upgrade` or somebody's kubectl edit puts the caBundle back to
// whatever the manifest said.
func TestAnOverwrittenCABundleIsRestored(t *testing.T) {
	ca := []byte("the real CA")
	cfg := validatingConfig("quota.odooenvironment.doblura.dev")
	cfg.Webhooks[0].ClientConfig.CABundle = []byte("a stale CA from a previous release")
	c := admissionClient(t, cfg)
	r := &CABundleReconciler{Client: c, Name: "doblura-quota", CABundle: ca}

	if _, err := r.Reconcile(context.Background(), reconcileFor("doblura-quota")); err != nil {
		t.Fatal(err)
	}
	var got admissionregistrationv1.ValidatingWebhookConfiguration
	if err := c.Get(context.Background(), client.ObjectKey{Name: "doblura-quota"}, &got); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got.Webhooks[0].ClientConfig.CABundle, ca) {
		t.Fatal("a stale caBundle was left in place")
	}
}

// And a reconcile that has nothing to do must write nothing: a controller that
// patches on every pass fights the API server for no reason.
func TestAnAlreadyCorrectCABundleIsNotWrittenAgain(t *testing.T) {
	ca := []byte("the real CA")
	cfg := validatingConfig("quota.odooenvironment.doblura.dev")
	cfg.Webhooks[0].ClientConfig.CABundle = ca
	c := admissionClient(t, cfg)
	r := &CABundleReconciler{Client: c, Name: "doblura-quota", CABundle: ca}

	before := readVersion(t, c, "doblura-quota")
	if _, err := r.Reconcile(context.Background(), reconcileFor("doblura-quota")); err != nil {
		t.Fatal(err)
	}
	if after := readVersion(t, c, "doblura-quota"); after != before {
		t.Errorf("the object was rewritten with no change: %s -> %s", before, after)
	}
}

// The mutating configuration is a different kind, and the reconciler is
// instantiated twice rather than guessing from a request that carries no kind.
func TestTheMutatingConfigurationIsPublishedToo(t *testing.T) {
	ca := []byte("the real CA")
	c := admissionClient(t, &admissionregistrationv1.MutatingWebhookConfiguration{
		ObjectMeta: metav1.ObjectMeta{Name: "doblura-creator"},
		Webhooks: []admissionregistrationv1.MutatingWebhook{
			{Name: "creator.odooenvironment.doblura.dev"},
		},
	})
	r := &CABundleReconciler{Client: c, Name: "doblura-creator", Mutating: true, CABundle: ca}

	if _, err := r.Reconcile(context.Background(), reconcileFor("doblura-creator")); err != nil {
		t.Fatal(err)
	}
	var got admissionregistrationv1.MutatingWebhookConfiguration
	if err := c.Get(context.Background(), client.ObjectKey{Name: "doblura-creator"}, &got); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got.Webhooks[0].ClientConfig.CABundle, ca) {
		t.Fatal("the mutating configuration was left without the CA, and with it the creator stamp stops working")
	}
}

// Somebody else's webhook configuration is none of the operator's business. The
// watch filters them out; this asserts the handler refuses them too, because a
// predicate is easy to lose in a refactor and this is a cluster-scoped write.
func TestAnotherConfigurationIsLeftAlone(t *testing.T) {
	cfg := validatingConfig("someone.else.example.com")
	cfg.Name = "cert-manager-webhook"
	c := admissionClient(t, cfg)
	r := &CABundleReconciler{Client: c, Name: "doblura-quota", CABundle: []byte("ours")}

	if _, err := r.Reconcile(context.Background(), reconcileFor("cert-manager-webhook")); err != nil {
		t.Fatal(err)
	}
	var got admissionregistrationv1.ValidatingWebhookConfiguration
	if err := c.Get(context.Background(), client.ObjectKey{Name: "cert-manager-webhook"}, &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Webhooks[0].ClientConfig.CABundle) != 0 {
		t.Fatal("the operator rewrote a webhook configuration that is not its own")
	}
}

// A configuration that is not installed is not an error: the release may not ship
// the webhook, and the next create of the object wakes the controller up.
func TestAMissingConfigurationIsNotAnError(t *testing.T) {
	r := &CABundleReconciler{Client: admissionClient(t), Name: "doblura-quota", CABundle: []byte("ours")}
	if _, err := r.Reconcile(context.Background(), reconcileFor("doblura-quota")); err != nil {
		t.Fatalf("a missing configuration must not be an error: %v", err)
	}
}

func reconcileFor(name string) ctrl.Request {
	return ctrl.Request{NamespacedName: client.ObjectKey{Name: name}}
}

func readVersion(t *testing.T, c client.Client, name string) string {
	t.Helper()
	var cfg admissionregistrationv1.ValidatingWebhookConfiguration
	if err := c.Get(context.Background(), client.ObjectKey{Name: name}, &cfg); err != nil {
		t.Fatal(err)
	}
	return cfg.ResourceVersion
}

var _ reconcile.Reconciler = &CABundleReconciler{}
