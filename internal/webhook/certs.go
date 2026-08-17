// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Antoni Romera

package webhook

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// ─────────────── Certificates, without depending on cert-manager ───────────────
//
// A webhook has to be served over TLS with a certificate the API server trusts,
// and there are exactly two ways to arrange that.
//
// The chosen one: the operator issues its own CA at startup, keeps it in a Secret,
// and a controller stamps the public half into the caBundle of its own webhook
// configurations. Roughly a hundred lines of standard-library crypto, in this file
// and cabundle.go.
//
// The one not chosen: require cert-manager, annotate the webhook configuration and
// let it inject the caBundle. It is less code here and more elsewhere. cert-manager
// would become a hard prerequisite of `helm install doblura`, which means the first
// experience of anyone whose cluster does not already have it is an install that
// fails on a missing CRD, followed by installing an unrelated operator to run an
// ERP tool. Doblura's user is a delivery team with one cluster, not a platform team
// with a base layer. A dependency that big has to buy more than a hundred lines.
//
// Making it a chart value — cert-manager if you have it, self-signed if you do not
// — was the third option, and it is the worst of the three: two certificate paths,
// only one of which ever gets tested, and the untested one discovered during an
// incident. One path, always taken.
//
// What is deliberately NOT here
// ─────────────────────────────
// Rotation on a timer. The certificate is issued for ten years and re-issued when
// fewer than thirty days remain, which in practice means on some manager restart.
// The rotation machinery a ninety-day certificate would force — reloading the
// serving certificate in every replica, re-stamping the caBundle, and tolerating
// the window where a replica and the caBundle disagree — is a lot of code
// defending against nothing: this CA's only trust anchor is a caBundle in
// Doblura's own webhook configuration. There is no third party to lose trust in
// it, and no CRL anybody consults.
//
// The rotation procedure, should it ever be needed, is: delete the Secret and
// restart the manager. That is documented rather than automated on purpose.

const (
	certKey = "tls.crt"
	keyKey  = "tls.key"
	caKey   = "ca.crt"

	// certLifetime is long because see above.
	certLifetime = 10 * 365 * 24 * time.Hour
	// renewBefore re-issues a certificate that is nearly expired. It exists for
	// the case where somebody put a short-lived certificate in the Secret by
	// hand, not for the ten-year one this code issues.
	renewBefore = 30 * 24 * time.Hour
)

// CertOptions says who the certificate is for.
type CertOptions struct {
	// Namespace and ServiceName produce the DNS names the API server will use to
	// reach the webhook. Getting them wrong is not a subtle failure: every
	// request fails the TLS handshake, and with failurePolicy Fail that is every
	// OdooEnvironment create refused.
	Namespace   string
	ServiceName string
	SecretName  string
}

// Bundle is a serving certificate and the CA that signed it.
type Bundle struct {
	Certificate tls.Certificate
	// CAPEM is what goes into the webhook configuration's caBundle.
	CAPEM []byte
}

// Apply installs the bundle into a TLS configuration.
//
// Handing controller-runtime a GetCertificate function is what keeps the key in
// memory: its default path is a certwatcher over files on disk, which would mean
// writing the key to the container's filesystem and mounting somewhere writable
// for it to live. Nothing needs the key on disk, so it never goes there.
func (b *Bundle) Apply(cfg *tls.Config) {
	cfg.MinVersion = tls.VersionTLS12
	cfg.GetCertificate = func(*tls.ClientHelloInfo) (*tls.Certificate, error) {
		return &b.Certificate, nil
	}
}

// EnsureServingCert returns the serving certificate, issuing and storing one if
// there is not already a usable one in the Secret.
//
// The Secret is what makes more than one replica work. Each replica generating its
// own CA would be fine right up until the second one answered a request: the
// Service load-balances, the caBundle holds one CA, and every request routed to
// the other replica fails the handshake. So the Secret is the shared state, and
// whichever replica gets there first decides.
func EnsureServingCert(ctx context.Context, c client.Client, o CertOptions) (*Bundle, error) {
	names := dnsNames(o)

	// Bounded retries rather than a plain get-or-create: two replicas starting
	// together race, and the loser of either race resolves it by reading what the
	// winner wrote.
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		var sec corev1.Secret
		err := c.Get(ctx, client.ObjectKey{Namespace: o.Namespace, Name: o.SecretName}, &sec)
		switch {
		case err == nil:
			if b, ok := adopt(&sec, names); ok {
				return b, nil
			}
			// Present but not usable: expired, corrupt, or issued for a
			// different Service name. Re-issue in place.
			fresh, data, err := issue(names)
			if err != nil {
				return nil, err
			}
			sec.Data = data
			if err := c.Update(ctx, &sec); err != nil {
				if errors.IsConflict(err) {
					lastErr = err
					continue
				}
				return nil, fmt.Errorf("replacing the webhook serving certificate: %w", err)
			}
			return fresh, nil

		case errors.IsNotFound(err):
			fresh, data, err := issue(names)
			if err != nil {
				return nil, err
			}
			sec := corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{
					Name:      o.SecretName,
					Namespace: o.Namespace,
					Labels: map[string]string{
						"app.kubernetes.io/name":       "doblura",
						"app.kubernetes.io/component":  "webhook",
						"app.kubernetes.io/managed-by": "doblura-manager",
					},
				},
				Type: corev1.SecretTypeTLS,
				Data: data,
			}
			if err := c.Create(ctx, &sec); err != nil {
				if errors.IsAlreadyExists(err) {
					// Another replica won. Go round and read theirs: the point of
					// the Secret is that everyone serves the same certificate.
					lastErr = err
					continue
				}
				return nil, fmt.Errorf("storing the webhook serving certificate: %w", err)
			}
			return fresh, nil

		default:
			return nil, fmt.Errorf("reading the webhook serving certificate: %w", err)
		}
	}
	return nil, fmt.Errorf("could not settle the webhook serving certificate after 3 attempts: %w", lastErr)
}

// dnsNames is every name the API server might use to reach the Service.
//
// All four forms, because which one is used depends on how the webhook
// configuration was written and on the cluster's DNS domain, and a missing SAN
// shows up as a handshake failure rather than as anything that says "SAN".
func dnsNames(o CertOptions) []string {
	return []string{
		o.ServiceName,
		o.ServiceName + "." + o.Namespace,
		o.ServiceName + "." + o.Namespace + ".svc",
		o.ServiceName + "." + o.Namespace + ".svc.cluster.local",
	}
}

// adopt reads a stored certificate and reports whether it can still be served.
func adopt(sec *corev1.Secret, names []string) (*Bundle, bool) {
	certPEM, keyPEM, caPEM := sec.Data[certKey], sec.Data[keyKey], sec.Data[caKey]
	if len(certPEM) == 0 || len(keyPEM) == 0 || len(caPEM) == 0 {
		return nil, false
	}
	pair, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return nil, false
	}
	leaf, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil {
		return nil, false
	}
	if time.Now().Add(renewBefore).After(leaf.NotAfter) {
		return nil, false
	}
	// A Service that was renamed leaves a certificate that is valid and useless.
	// Checking the names here is what turns that into a re-issue instead of a
	// handshake failure nobody can explain.
	for _, n := range names {
		if leaf.VerifyHostname(n) != nil {
			return nil, false
		}
	}
	pair.Leaf = leaf
	return &Bundle{Certificate: pair, CAPEM: caPEM}, true
}

// issue mints a CA and a serving certificate signed by it.
//
// Two certificates rather than one self-signed leaf: the CA is what the API server
// trusts, so a future leaf can be replaced without touching the caBundle of every
// webhook configuration. It costs about ten lines and it is the difference between
// being able to rotate and not.
func issue(names []string) (*Bundle, map[string][]byte, error) {
	return issueFor(names, certLifetime)
}

// issueFor is issue with the lifetime as a parameter, so a test can produce the
// nearly-expired certificate the renewal path exists for. Nothing else should call
// it: the lifetime is a decision, argued above, not a knob.
func issueFor(names []string, lifetime time.Duration) (*Bundle, map[string][]byte, error) {
	now := time.Now()
	// Backdated by an hour. Clock skew between the node that issued the
	// certificate and the API server that validates it is real, and a
	// not-yet-valid certificate fails in exactly the same way as a wrong one.
	notBefore := now.Add(-time.Hour)

	caKeyPair, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	serial, err := serialNumber()
	if err != nil {
		return nil, nil, err
	}
	caTemplate := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "doblura-webhook-ca", Organization: []string{"doblura.dev"}},
		NotBefore:             notBefore,
		NotAfter:              now.Add(lifetime),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
		IsCA:                  true,
		MaxPathLenZero:        true,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTemplate, caTemplate, &caKeyPair.PublicKey, caKeyPair)
	if err != nil {
		return nil, nil, err
	}
	ca, err := x509.ParseCertificate(caDER)
	if err != nil {
		return nil, nil, err
	}

	leafKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	serial, err = serialNumber()
	if err != nil {
		return nil, nil, err
	}
	leafTemplate := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: names[len(names)-1]},
		NotBefore:    notBefore,
		NotAfter:     now.Add(lifetime),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     names,
	}
	leafDER, err := x509.CreateCertificate(rand.Reader, leafTemplate, ca, &leafKey.PublicKey, caKeyPair)
	if err != nil {
		return nil, nil, err
	}

	leafKeyDER, err := x509.MarshalECPrivateKey(leafKey)
	if err != nil {
		return nil, nil, err
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: leafDER})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: leafKeyDER})
	caPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER})

	pair, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return nil, nil, err
	}
	leaf, err := x509.ParseCertificate(leafDER)
	if err != nil {
		return nil, nil, err
	}
	pair.Leaf = leaf

	// The CA's private key is thrown away with this function's stack frame. It
	// signs one certificate and is never needed again, and a key that does not
	// exist cannot be stolen from a Secret.
	return &Bundle{Certificate: pair, CAPEM: caPEM},
		map[string][]byte{certKey: certPEM, keyKey: keyPEM, caKey: caPEM},
		nil
}

func serialNumber() (*big.Int, error) {
	return rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
}
