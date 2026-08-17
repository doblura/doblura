// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Antoni Romera

package webhook

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func certOpts() CertOptions {
	return CertOptions{Namespace: "doblura-system", ServiceName: "doblura-webhook", SecretName: "doblura-webhook-cert"}
}

func coreClient(t *testing.T, objs ...client.Object) client.Client {
	t.Helper()
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	return fake.NewClientBuilder().WithScheme(s).WithObjects(objs...).Build()
}

// The whole point of generating our own: what the API server is told to trust has
// to actually verify what the webhook serves, for the name the API server dials.
func TestTheServingCertificateVerifiesAgainstThePublishedCA(t *testing.T) {
	c := coreClient(t)

	bundle, err := EnsureServingCert(context.Background(), c, certOpts())
	if err != nil {
		t.Fatal(err)
	}

	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(bundle.CAPEM) {
		t.Fatal("the published caBundle is not a PEM certificate")
	}
	leaf, err := x509.ParseCertificate(bundle.Certificate.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}

	// Every name the API server might dial, verified as a client would: this is
	// the check that catches a missing SAN, which otherwise surfaces as an
	// unexplained handshake failure at admission time.
	for _, name := range []string{
		"doblura-webhook",
		"doblura-webhook.doblura-system",
		"doblura-webhook.doblura-system.svc",
		"doblura-webhook.doblura-system.svc.cluster.local",
	} {
		if _, err := leaf.Verify(x509.VerifyOptions{
			DNSName:   name,
			Roots:     pool,
			KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		}); err != nil {
			t.Errorf("the certificate does not verify for %s: %v", name, err)
		}
	}

	// And a name it is NOT for must fail, or the test above proves nothing.
	if _, err := leaf.Verify(x509.VerifyOptions{
		DNSName: "somebody-elses-webhook.default.svc", Roots: pool,
		KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}); err == nil {
		t.Error("the certificate verified for a name it was not issued for")
	}
}

// More than one replica has to serve the SAME certificate: the Service
// load-balances and the caBundle holds one CA.
func TestASecondReplicaAdoptsTheStoredCertificate(t *testing.T) {
	c := coreClient(t)

	first, err := EnsureServingCert(context.Background(), c, certOpts())
	if err != nil {
		t.Fatal(err)
	}
	second, err := EnsureServingCert(context.Background(), c, certOpts())
	if err != nil {
		t.Fatal(err)
	}

	if !bytes.Equal(first.CAPEM, second.CAPEM) {
		t.Error("the second replica issued its own CA: every request routed to it would fail the handshake")
	}
	if !bytes.Equal(first.Certificate.Certificate[0], second.Certificate.Certificate[0]) {
		t.Error("the second replica issued its own serving certificate")
	}
}

func TestAnUnusableStoredCertificateIsReplaced(t *testing.T) {
	nearlyExpired, expiring, err := issueFor(dnsNames(certOpts()), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	_, wrongService, err := issueFor(dnsNames(CertOptions{Namespace: "doblura-system", ServiceName: "renamed"}), certLifetime)
	if err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name string
		data map[string][]byte
	}{
		{"about to expire", expiring},
		{"issued for a Service that has since been renamed", wrongService},
		{"corrupt", map[string][]byte{certKey: []byte("not a certificate"), keyKey: []byte("nor a key"), caKey: []byte("x")}},
		{"missing the CA", map[string][]byte{certKey: expiring[certKey], keyKey: expiring[keyKey]}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			o := certOpts()
			c := coreClient(t, &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{Name: o.SecretName, Namespace: o.Namespace},
				Type:       corev1.SecretTypeTLS,
				Data:       tc.data,
			})

			bundle, err := EnsureServingCert(context.Background(), c, o)
			if err != nil {
				t.Fatal(err)
			}
			if bytes.Equal(bundle.Certificate.Certificate[0], nearlyExpired.Certificate.Certificate[0]) {
				t.Fatal("the unusable certificate was adopted instead of replaced")
			}

			// And the replacement was written back, or the next replica would
			// disagree with this one.
			var stored corev1.Secret
			if err := c.Get(context.Background(),
				client.ObjectKey{Namespace: o.Namespace, Name: o.SecretName}, &stored); err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(stored.Data[caKey], bundle.CAPEM) {
				t.Error("the Secret does not hold the certificate now being served")
			}
		})
	}
}

func TestTheBundleConfiguresTLS(t *testing.T) {
	bundle, err := EnsureServingCert(context.Background(), coreClient(t), certOpts())
	if err != nil {
		t.Fatal(err)
	}

	cfg := &tls.Config{} //nolint:gosec // MinVersion is what Apply is being tested for
	bundle.Apply(cfg)

	if cfg.MinVersion != tls.VersionTLS12 {
		t.Error("TLS 1.0 and 1.1 must not be offered")
	}
	if cfg.GetCertificate == nil {
		t.Fatal("without GetCertificate, controller-runtime falls back to reading the key off disk")
	}
	got, err := cfg.GetCertificate(&tls.ClientHelloInfo{ServerName: "doblura-webhook.doblura-system.svc"})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got.Certificate[0], bundle.Certificate.Certificate[0]) {
		t.Error("the TLS configuration serves a different certificate than the one published")
	}
}
