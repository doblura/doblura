// SPDX-License-Identifier: Apache-2.0
// Copyright (C) 2026 Antoni Romera

package v1alpha1

import "testing"

// Hardening a public environment must include the three controls that separate
// it from being a leak.
func TestPublicEnvironmentHardeningIsComplete(t *testing.T) {
	yes := true
	s := &OdooEnvironmentSpec{
		Data:     EnvData{Type: DataSnapshot},
		Exposure: EnvExposure{Public: &yes, Host: "pr-1234.lab.example.com"},
	}
	got := map[string]bool{}
	for _, h := range s.RequiredHardening() {
		got[h] = true
	}
	for _, want := range []string{
		"randomize-user-passwords",   // or we serve the dump's known password
		"strip-external-credentials", // the gap neutralization does not cover
		"deny-egress",                // so it cannot reach the internal network
		"ingress-auth", "no-index", "rate-limit",
	} {
		if !got[want] {
			t.Errorf("a public environment requires the %q control", want)
		}
	}
}

// A private environment does not need the ingress controls, but it DOES need the
// internal ones: the dump carries real data even if nobody from outside reaches
// it.
func TestPrivateEnvironmentKeepsInternalControls(t *testing.T) {
	s := &OdooEnvironmentSpec{Data: EnvData{Type: DataSnapshot}}
	got := map[string]bool{}
	for _, h := range s.RequiredHardening() {
		got[h] = true
	}
	if !got["strip-external-credentials"] {
		t.Error("stripping external credentials is needed even for a private environment")
	}
	if got["ingress-auth"] {
		t.Error("a private environment does not need ingress auth")
	}
	if s.IsPublic() {
		t.Error("without exposure.public it is not public")
	}
}

// Explicitly disabling a control is honoured: the API does not lie about what it
// will do. The CEL guardrails are what stop you doing it on a public
// environment.
func TestAControlCanBeDisabledExplicitly(t *testing.T) {
	no := false
	s := &OdooEnvironmentSpec{
		Data:     EnvData{Type: DataDemo},
		Security: EnvSecurity{RandomizeUserPasswords: &no, StripExternalCredentials: &no},
	}
	for _, h := range s.RequiredHardening() {
		if h == "randomize-user-passwords" || h == "strip-external-credentials" {
			t.Errorf("%q was disabled explicitly and must not be applied", h)
		}
	}
}
