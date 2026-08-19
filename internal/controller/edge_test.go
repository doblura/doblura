// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Antoni Romera

package controller

import (
	"sort"
	"strings"
	"testing"

	doblurav1alpha1 "github.com/doblura/doblura/api/v1alpha1"
)

func publicEnv() *doblurav1alpha1.OdooEnvironment {
	yes := true
	env := &doblurav1alpha1.OdooEnvironment{}
	env.Name = "acme-staging"
	env.Namespace = "demo"
	env.Spec.Exposure.Public = &yes
	env.Spec.Exposure.Host = "staging-ab12cd.acme.example"
	env.Spec.Exposure.Auth.Type = doblurav1alpha1.IngressAuthBasic
	rps := int32(20)
	env.Spec.Exposure.RateLimitRPS = &rps
	return env
}

// Every middleware the Ingress names must be one the controller creates.
//
// This is the whole reason edge.go exists. The Ingress annotation referred to
// -basicauth, -noindex and -ratelimit; nothing in doblura created any of them;
// Traefik logged `middleware "demo-x-ratelimit@kubernetescrd" does not exist` on
// every reconcile; and a public environment's authentication was applied by
// nobody. The two lists are now generated from one function, and this asserts they
// stay that way.
func TestEveryReferencedMiddlewareIsAlsoCreated(t *testing.T) {
	env := publicEnv()
	const secret = "acme-staging-edge-auth"

	created := map[string]bool{}
	for _, r := range edgeRules(env, secret) {
		created[edgeObjectName(env, r.Name)] = true
	}

	referenced := edgeMiddlewareNames(env, secret)
	if len(referenced) == 0 {
		t.Fatal("a public environment with auth, noindex and a rate limit " +
			"references no middleware at all")
	}
	for _, ref := range referenced {
		// demo-acme-staging-basicauth@kubernetescrd -> acme-staging-basicauth
		name := strings.TrimSuffix(ref, "@kubernetescrd")
		name = strings.TrimPrefix(name, env.Namespace+"-")
		if !created[name] {
			t.Errorf("the Ingress refers to %q and nothing creates it — Traefik "+
				"will report the router as broken and the rule will not apply", ref)
		}
	}
}

// A public environment always gets authentication in front of it.
func TestAPublicEnvironmentIsNeverLeftOpen(t *testing.T) {
	env := publicEnv()
	rules := edgeRules(env, "acme-staging-edge-auth")

	var auth map[string]any
	for _, r := range rules {
		if r.Name == "basicauth" {
			auth, _ = r.Spec["basicAuth"].(map[string]any)
		}
	}
	if auth == nil {
		t.Fatal("a public environment with BasicAuth produces no basic auth rule")
	}
	if auth["secret"] != "acme-staging-edge-auth" {
		t.Fatalf("basic auth reads secret %v, not the one that was created — a "+
			"middleware pointing at a Secret that does not exist fails open or "+
			"closed depending on the version", auth["secret"])
	}
}

// HSTS never claims subdomains, and never asks for preload.
//
// The setting a browser remembers for a year and that cannot be taken back.
// Environments live on generated names under a customer's domain, so a claim over
// the whole domain from one of them reaches names doblura did not create.
func TestHSTSDoesNotClaimTheWholeDomain(t *testing.T) {
	env := publicEnv()
	var headers map[string]any
	for _, r := range edgeRules(env, "s") {
		if r.Name == "hsts" {
			headers, _ = r.Spec["headers"].(map[string]any)
		}
	}
	if headers == nil {
		t.Fatal("a public environment sends no HSTS header")
	}
	if headers["stsIncludeSubdomains"] != false {
		t.Error("HSTS includes subdomains: one environment would make every other " +
			"name under the customer's domain https-only, for a year, including " +
			"names doblura does not manage")
	}
	if headers["stsPreload"] != false {
		t.Error("HSTS asks for preload, which is a submission to browser vendors " +
			"that is very hard to undo")
	}
}

// Turning a rule off removes it from what is referenced.
func TestARuleTurnedOffIsNotReferenced(t *testing.T) {
	env := publicEnv()
	no := false
	env.Spec.Exposure.NoIndex = &no
	zero := int32(0)
	env.Spec.Exposure.RateLimitRPS = &zero

	for _, ref := range edgeMiddlewareNames(env, "s") {
		if strings.Contains(ref, "noindex") || strings.Contains(ref, "ratelimit") {
			t.Errorf("%s is still referenced after being turned off", ref)
		}
	}
}

// An allowlist becomes a real source range.
func TestTheAllowListReachesTheEdge(t *testing.T) {
	env := publicEnv()
	env.Spec.Exposure.AllowFrom = []string{"203.0.113.0/24", " 198.51.100.7/32 "}

	var got []any
	for _, r := range edgeRules(env, "s") {
		if r.Name == "allowfrom" {
			list, _ := r.Spec["ipAllowList"].(map[string]any)
			got, _ = list["sourceRange"].([]any)
		}
	}
	if len(got) != 2 {
		t.Fatalf("the allowlist produced %d ranges, not 2: %v", len(got), got)
	}
	// Trimmed: a range with a stray space is rejected by Traefik and the whole
	// middleware fails, which takes the environment down rather than letting one
	// range through.
	if got[1] != "198.51.100.7/32" {
		t.Errorf("a range kept its surrounding spaces: %q", got[1])
	}
}

// The basic auth Secret has exactly one key.
//
// Traefik refuses more than one — "found 2 elements for secret ..., must be single
// element exactly" — and a middleware that fails to load makes its router invalid,
// so the environment answers 404 on every path with the reason only in the proxy's
// log. The first version put the hash and the plaintext together, which is what
// anybody would do, and it took the whole environment down.
func TestTheBasicAuthSecretHasExactlyOneKey(t *testing.T) {
	env := publicEnv()
	auth, shown := htpasswdSecrets(env, "acme-staging-edge-auth", "$2a$12$hash", "s3cret")

	if len(auth.StringData) != 1 {
		t.Fatalf("the htpasswd secret carries %d keys (%v); Traefik requires "+
			"exactly one and refuses the whole middleware otherwise",
			len(auth.StringData), keysOf(auth.StringData))
	}
	if _, ok := auth.StringData["users"]; !ok {
		t.Fatalf("the one key is %v, not \"users\" — the only name Traefik's "+
			"basicAuth reads", keysOf(auth.StringData))
	}
	if !strings.HasPrefix(auth.StringData["users"], edgeUser+":") {
		t.Errorf("the htpasswd line is %q, which is not <user>:<hash>",
			auth.StringData["users"])
	}

	// And the password has to exist somewhere, or the generated address is a
	// lockout rather than an inconvenience.
	if shown.StringData["password"] != "s3cret" {
		t.Error("the password is not recorded anywhere readable, so nobody can be " +
			"given access to the environment")
	}
	if shown.Name == auth.Name {
		t.Error("both secrets have the same name, so one overwrites the other")
	}
}

func keysOf(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Long polling is excluded before any rule can deny it.
//
// A websocket connection is an HTTP request that never finishes. Inspecting it
// holds it until something times out or refuses it outright, and Odoo then loses
// chat, notifications and every live update while every page still loads — so it
// reads as "Odoo is broken", not as "the firewall we switched on last Tuesday".
// The exclusion has to come first, because a rule that runs after a deny never
// runs at all.
func TestTheWAFNeverInspectsLongPolling(t *testing.T) {
	yes := true
	waf := &doblurav1alpha1.EnvWAF{
		Mode:            doblurav1alpha1.WAFInCluster,
		Enforcement:     doblurav1alpha1.WAFBlock,
		BlockRPC:        &yes,
		ExtraDirectives: []string{`SecRule REQUEST_URI "@contains x" "id:9999,phase:1,deny"`},
	}
	d := corazaDirectives(waf)

	firstDeny, wsExclusion, lpExclusion := -1, -1, -1
	for i, line := range d {
		switch {
		case strings.Contains(line, "/websocket"):
			wsExclusion = i
		case strings.Contains(line, "/longpolling/"):
			lpExclusion = i
		case strings.Contains(line, "deny") && firstDeny < 0:
			firstDeny = i
		}
	}

	if wsExclusion < 0 || lpExclusion < 0 {
		t.Fatal("long polling is not excluded from inspection at all; the WAF will " +
			"hold or refuse websocket connections and Odoo will lose chat and " +
			"notifications while every page still loads")
	}
	if firstDeny >= 0 && (wsExclusion > firstDeny || lpExclusion > firstDeny) {
		t.Fatalf("a deny rule (line %d) comes before the long-poll exclusions "+
			"(%d, %d); rules after a deny never run", firstDeny, wsExclusion, lpExclusion)
	}
}

// The database manager is closed by default, and it is the second lock.
func TestTheDatabaseManagerIsClosedByDefault(t *testing.T) {
	waf := &doblurav1alpha1.EnvWAF{Mode: doblurav1alpha1.WAFInCluster}
	joined := strings.Join(corazaDirectives(waf), "\n")
	if !strings.Contains(joined, "/web/database/") {
		t.Error("the database manager is reachable: /web/database/* creates, drops " +
			"and restores databases")
	}
	// And RPC is NOT closed by default: the external API is how integrations and
	// the customer's own scripts talk to Odoo.
	if strings.Contains(joined, "/jsonrpc") {
		t.Error("RPC is closed by default, which breaks every integration silently")
	}
}

// Detect means detect.
func TestDetectDoesNotBlock(t *testing.T) {
	waf := &doblurav1alpha1.EnvWAF{
		Mode: doblurav1alpha1.WAFInCluster, Enforcement: doblurav1alpha1.WAFDetect,
	}
	if !strings.Contains(corazaDirectives(waf)[0], "DetectionOnly") {
		t.Fatalf("Detect enforcement still starts the engine in blocking mode: %q",
			corazaDirectives(waf)[0])
	}
}

// The request body is never buffered.
//
// Odoo moves attachments and database dumps through ordinary POSTs, and a WAF that
// buffers those to inspect them turns a 200 MB restore into the proxy's memory
// problem.
func TestTheWAFDoesNotBufferBodies(t *testing.T) {
	waf := &doblurav1alpha1.EnvWAF{Mode: doblurav1alpha1.WAFInCluster}
	if !strings.Contains(strings.Join(corazaDirectives(waf), "\n"), "SecRequestBodyAccess Off") {
		t.Error("request bodies are buffered for inspection; an Odoo restore or a " +
			"large attachment becomes the proxy's memory problem")
	}
}
