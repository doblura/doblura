// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Antoni Romera

package console

import "testing"

// The return path after a bounce cannot leave this site.
//
// It appears on the sign-in page, which is the single best place in any
// application to put an open redirector: somebody who can choose where "sign in"
// sends you can send you to a page that looks exactly like this one.
func TestTheReturnPathCannotLeaveTheSite(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{"/customers", "/customers"},
		{"/o/environments?x=1", "/o/environments?x=1"},
		{"", "/"},
		{"//evil.example.com", "/"},       // protocol-relative: a browser follows it off-site
		{"https://evil.example.com", "/"}, // absolute
		{"evil.example.com", "/"},         // no leading slash
		{"/auth/login", "/"},              // would loop
		{"/auth/callback?code=x", "/"},    // would loop, and replays a code
	} {
		if got := safeNext(c.in); got != c.want {
			t.Errorf("safeNext(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// A scope is a namespace name and nothing else.
//
// It goes into a cookie and comes back out into an API call, and a value that
// reaches a request unchecked is a value worth checking.
func TestAScopeIsANamespaceName(t *testing.T) {
	ok := []string{"demo", "acme", "a", "a-b-c", "customer-42"}
	bad := []string{"", "Demo", "-demo", "demo-", "de mo", "demo/../other", "*", "demo\n"}

	for _, v := range ok {
		if !scopeNamePattern.MatchString(v) {
			t.Errorf("%q should be a usable scope", v)
		}
	}
	for _, v := range bad {
		if scopeNamePattern.MatchString(v) {
			t.Errorf("%q should NOT be accepted as a scope", v)
		}
	}
}
