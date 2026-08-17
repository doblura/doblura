// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Antoni Romera

package webhook

import (
	"fmt"
	"strings"

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

// Options is everything the webhook is told from the command line. The chart fills
// every field in, so nothing here is guessed from the environment: a webhook that
// infers its own Service name is one that fails a TLS handshake the day somebody
// renames the release.
type Options struct {
	// Port of the webhook server. Zero switches the whole thing off, which is what
	// a release with replicaCount 0 wants — there is nothing to serve it.
	Port int

	Namespace   string
	ServiceName string
	// CertSecretName holds the serving certificate. Shared by every replica; see
	// certs.go.
	CertSecretName string

	ValidatingConfigName string
	MutatingConfigName   string

	// ExemptUsers are identities the quota does not apply to, as they appear in
	// an AdmissionRequest: "system:serviceaccount:<namespace>:<name>" for a
	// ServiceAccount.
	ExemptUsers []string

	// MaxEnvironmentsPerCreator is the per-person, cluster-wide allowance.
	MaxEnvironmentsPerCreator int
}

// Enabled reports whether the webhook should be served at all.
func (o Options) Enabled() bool { return o.Port > 0 }

// Validate refuses a half-configured webhook at startup.
//
// Exiting is the right answer rather than starting without it: the webhook
// configurations are installed by the same chart that passes these flags, so a
// missing one means a fail-closed webhook is already in the cluster with nothing
// answering it. Crash-looping with a legible message beats serving a cluster where
// every environment create is refused for a reason nobody can find.
func (o Options) Validate() error {
	if !o.Enabled() {
		return nil
	}
	for name, value := range map[string]string{
		"--webhook-service":           o.ServiceName,
		"--webhook-namespace":         o.Namespace,
		"--webhook-cert-secret":       o.CertSecretName,
		"--validating-webhook-config": o.ValidatingConfigName,
		"--mutating-webhook-config":   o.MutatingConfigName,
	} {
		if strings.TrimSpace(value) == "" {
			return fmt.Errorf("%s is required when the webhook is enabled", name)
		}
	}
	return nil
}

// CertOptions is the subset the certificate cares about.
func (o Options) CertOptions() CertOptions {
	return CertOptions{
		Namespace:   o.Namespace,
		ServiceName: o.ServiceName,
		SecretName:  o.CertSecretName,
	}
}

// Register wires the two handlers and the two caBundle controllers into the
// manager.
func Register(mgr ctrl.Manager, o Options, caBundle []byte) error {
	decoder := admission.NewDecoder(mgr.GetScheme())
	server := mgr.GetWebhookServer()

	server.Register(MutatePath, &admission.Webhook{Handler: &EnvironmentCreator{Decoder: decoder}})
	server.Register(ValidatePath, &admission.Webhook{Handler: &EnvironmentQuota{
		// GetAPIReader, not GetClient: the count has to be current. See the
		// comment on EnvironmentQuota.Reader.
		Reader:        mgr.GetAPIReader(),
		Decoder:       decoder,
		ExemptUsers:   exemptSet(o.ExemptUsers),
		MaxPerCreator: int32(o.MaxEnvironmentsPerCreator), //nolint:gosec // a flag, bounded by the schema
	}})

	for _, r := range []*CABundleReconciler{
		{Client: mgr.GetClient(), Name: o.ValidatingConfigName, CABundle: caBundle},
		{Client: mgr.GetClient(), Name: o.MutatingConfigName, Mutating: true, CABundle: caBundle},
	} {
		if err := r.SetupWithManager(mgr); err != nil {
			return fmt.Errorf("registering the caBundle controller for %s: %w", r.Name, err)
		}
	}
	return nil
}

// exemptSet turns the flag's list into a lookup, dropping the empty strings a
// trailing comma leaves behind. An empty entry would exempt the unauthenticated
// identity, which is a bypass rather than a typo.
func exemptSet(users []string) map[string]bool {
	out := map[string]bool{}
	for _, u := range users {
		if u = strings.TrimSpace(u); u != "" {
			out[u] = true
		}
	}
	return out
}
