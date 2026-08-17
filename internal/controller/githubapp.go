// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Antoni Romera

package controller

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	doblurav1alpha1 "github.com/doblura/doblura/api/v1alpha1"
)

// MintedTokenSecretName is the ephemeral Secret where the operator leaves the
// installation token minted for one repo.
//
// It is owned by the OdooRehearsal, so garbage collection removes it along with
// the parent: an installation token lives one hour, and we do not want anything
// hanging around longer than necessary.
func MintedTokenSecretName(repoName string) string {
	return "doblura-gh-token-" + repoName
}

// ensureGitHubAppTokens mints whichever installation tokens are needed.
//
// Called before creating each phase's Job. GitHub App tokens expire after an
// hour and a migration phase can last longer, so we re-mint per phase rather
// than once at the start: every Job begins with a fresh token. Only the init
// container uses it, and it clones within the first seconds, so an hour is more
// than enough.
func (r *OdooRehearsalReconciler) ensureGitHubAppTokens(
	ctx context.Context,
	reh *doblurav1alpha1.OdooRehearsal,
) error {
	for _, repo := range reh.Spec.Addons.Repos {
		if !repo.Auth.NeedsTokenMinting() {
			continue
		}

		var src corev1.Secret
		key := client.ObjectKey{Namespace: reh.Namespace, Name: repo.Auth.SecretRef}
		if err := r.Get(ctx, key, &src); err != nil {
			return fmt.Errorf("reading GitHub App credentials from %q: %w", repo.Auth.SecretRef, err)
		}

		appID := strings.TrimSpace(string(src.Data["appID"]))
		installID := strings.TrimSpace(string(src.Data["installationID"]))
		pemKey := src.Data["privateKey"]
		if appID == "" || installID == "" || len(pemKey) == 0 {
			return fmt.Errorf(
				"GitHub App Secret %q must contain the appID, installationID and privateKey keys",
				repo.Auth.SecretRef)
		}

		token, expiry, err := mintInstallationToken(ctx, appID, installID, pemKey)
		if err != nil {
			return fmt.Errorf("minting the installation token for repo %q: %w", repo.Name, err)
		}

		sec := &corev1.Secret{
			TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Secret"},
			ObjectMeta: metav1.ObjectMeta{
				Name:      MintedTokenSecretName(repo.Name),
				Namespace: reh.Namespace,
				Labels: map[string]string{
					"app.kubernetes.io/managed-by": "doblura",
					"doblura.dev/ephemeral":         "true",
				},
				Annotations: map[string]string{
					"doblura.dev/expires-at": expiry.UTC().Format(time.RFC3339),
				},
			},
			Type:       corev1.SecretTypeOpaque,
			StringData: map[string]string{"token": token},
		}
		if err := ctrl.SetControllerReference(reh, sec, r.Scheme); err != nil {
			return err
		}
		if err := r.Patch(ctx, sec, client.Apply, fieldOwner, client.ForceOwnership); err != nil {
			return fmt.Errorf("storing the minted token for %q: %w", repo.Name, err)
		}
	}
	return nil
}

// mintInstallationToken performs the GitHub App exchange: it signs a JWT with
// the App's private key and trades it for an installation token.
//
// Implemented by hand rather than pulling in a JWT library because the JWT
// GitHub asks for is minimal (RS256, three claims) and does not justify a
// dependency with its own CVE surface.
func mintInstallationToken(ctx context.Context, appID, installationID string, pemKey []byte) (string, time.Time, error) {
	jwt, err := signAppJWT(appID, pemKey)
	if err != nil {
		return "", time.Time{}, err
	}

	url := fmt.Sprintf("https://api.github.com/app/installations/%s/access_tokens", installationID)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, nil)
	if err != nil {
		return "", time.Time{}, err
	}
	req.Header.Set("Authorization", "Bearer "+jwt)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")

	resp, err := (&http.Client{Timeout: 20 * time.Second}).Do(req)
	if err != nil {
		return "", time.Time{}, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusCreated {
		// Without dumping the body: it could carry installation details.
		return "", time.Time{}, fmt.Errorf(
			"GitHub returned %s when exchanging the JWT; check appID, installationID, and that the App has access to the repo",
			resp.Status)
	}

	var out struct {
		Token     string    `json:"token"`
		ExpiresAt time.Time `json:"expires_at"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", time.Time{}, err
	}
	return out.Token, out.ExpiresAt, nil
}

// signAppJWT signs the RS256 JWT GitHub requires to authenticate the App.
func signAppJWT(appID string, pemKey []byte) (string, error) {
	key, err := parseRSAKey(pemKey)
	if err != nil {
		return "", err
	}

	now := time.Now()
	header := base64URL(`{"alg":"RS256","typ":"JWT"}`)
	// iat 60 seconds in the past: GitHub rejects JWTs from the future and node
	// clocks drift. exp at 9 minutes, below the 10-minute maximum.
	claims := base64URL(fmt.Sprintf(`{"iat":%d,"exp":%d,"iss":"%s"}`,
		now.Add(-60*time.Second).Unix(), now.Add(9*time.Minute).Unix(), appID))

	signingInput := header + "." + claims
	sum := sha256.Sum256([]byte(signingInput))
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, cryptoSHA256, sum[:])
	if err != nil {
		return "", fmt.Errorf("signing the JWT: %w", err)
	}
	return signingInput + "." + base64.RawURLEncoding.EncodeToString(sig), nil
}

// parseRSAKey accepts both formats GitHub and secret managers hand the key out
// in: PKCS#1 ("BEGIN RSA PRIVATE KEY") and PKCS#8 ("BEGIN PRIVATE KEY").
// Accepting only one is a classic cause of "invalid key" with no further
// clues.
func parseRSAKey(pemKey []byte) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode(pemKey)
	if block == nil {
		return nil, fmt.Errorf("privateKey is not valid PEM")
	}
	if k, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		return k, nil
	}
	any, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("privateKey is neither PKCS#1 nor PKCS#8: %w", err)
	}
	k, ok := any.(*rsa.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("privateKey is not RSA; GitHub Apps use RSA")
	}
	return k, nil
}

func base64URL(s string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(s))
}
