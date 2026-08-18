// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Antoni Romera

package controller

import (
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"

	doblurav1alpha1 "github.com/doblura/doblura/api/v1alpha1"
)

// Each mechanism has to reach the clone container, and each in its own way.
//
// The enum listed four and one of them — GitHubApp — was wired only into the
// rehearsal controller, so an environment using it produced a pod mounting a
// Secret nobody created. These assert the plumbing per mechanism, which is the
// part that silently differs.
func TestEveryAuthMechanismReachesTheCloneContainer(t *testing.T) {
	cases := []struct {
		name    string
		auth    *doblurav1alpha1.GitAuth
		wantEnv []string
		wantNot []string
	}{
		{
			name:    "public repo needs nothing",
			auth:    nil,
			wantNot: []string{"GIT_PASSWORD", "GIT_SSH_KEY"},
		},
		{
			name:    "token travels as the password over HTTPS",
			auth:    &doblurav1alpha1.GitAuth{Type: doblurav1alpha1.AuthToken, SecretRef: "s"},
			wantEnv: []string{"GIT_USER", "GIT_PASSWORD"},
		},
		{
			name:    "basic auth is a username and a password",
			auth:    &doblurav1alpha1.GitAuth{Type: doblurav1alpha1.AuthBasicAuth, SecretRef: "s"},
			wantEnv: []string{"GIT_USER", "GIT_PASSWORD"},
		},
		{
			name:    "an ssh key goes to a file, never to the URL",
			auth:    &doblurav1alpha1.GitAuth{Type: doblurav1alpha1.AuthSSHKey, SecretRef: "s"},
			wantEnv: []string{"GIT_SSH_KEY"},
			wantNot: []string{"GIT_PASSWORD"},
		},
		{
			name:    "a GitHub App reads the token the operator minted",
			auth:    &doblurav1alpha1.GitAuth{Type: doblurav1alpha1.AuthGitHubApp, SecretRef: "s"},
			wantEnv: []string{"GIT_USER", "GIT_PASSWORD"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := cloneContainer(doblurav1alpha1.AddonRepo{
				Name: "r", URL: "https://github.com/o/r.git", Ref: "main", Auth: tc.auth,
			})
			have := map[string]bool{}
			for _, e := range c.Env {
				have[e.Name] = true
			}
			for _, w := range tc.wantEnv {
				if !have[w] {
					t.Errorf("%s: missing %s", tc.name, w)
				}
			}
			for _, w := range tc.wantNot {
				if have[w] {
					t.Errorf("%s: should not set %s", tc.name, w)
				}
			}
		})
	}
}

// The minted-token Secret must be the one the operator writes, or the pod waits
// forever on a Secret that will never exist.
func TestGitHubAppReadsTheSecretTheOperatorWrites(t *testing.T) {
	repo := doblurav1alpha1.AddonRepo{
		Name: "private", URL: "https://github.com/acme/addons.git", Ref: "main",
		Auth: &doblurav1alpha1.GitAuth{Type: doblurav1alpha1.AuthGitHubApp, SecretRef: "app"},
	}
	c := cloneContainer(repo)

	var from string
	for _, e := range c.Env {
		if e.Name == "GIT_PASSWORD" && e.ValueFrom != nil && e.ValueFrom.SecretKeyRef != nil {
			from = e.ValueFrom.SecretKeyRef.Name
		}
	}
	if want := MintedTokenSecretName(repo.Name); from != want {
		t.Errorf("the clone container reads %q but the operator writes %q", from, want)
	}
}

// The credential must not be readable by anything but the clone container.
func TestNoAuthSecretIsMountedAsAVolume(t *testing.T) {
	// A Secret mounted as a volume would be visible to every container in the
	// pod, Odoo included; as an env var on the init container it is not.
	c := cloneContainer(doblurav1alpha1.AddonRepo{
		Name: "r", URL: "https://github.com/o/r.git", Ref: "main",
		Auth: &doblurav1alpha1.GitAuth{Type: doblurav1alpha1.AuthToken, SecretRef: "s"},
	})
	for _, m := range c.VolumeMounts {
		if strings.Contains(m.Name, "auth") || strings.Contains(m.Name, "secret") {
			t.Errorf("the credential is mounted as a volume: %v", m)
		}
	}
	if len(c.Env) == 0 {
		t.Fatal("no credential reaches the container at all")
	}
	for _, e := range c.Env {
		if e.ValueFrom == nil {
			continue
		}
		if e.ValueFrom.SecretKeyRef == nil && e.ValueFrom.FieldRef == nil {
			t.Errorf("%s comes from something unexpected: %+v", e.Name, e.ValueFrom)
		}
	}
	_ = corev1.EnvVar{}
}
