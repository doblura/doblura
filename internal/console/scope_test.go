// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Antoni Romera

package console

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

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

// A refusal that is really a question gets asked, and one that is not does not.
//
// The case: somebody whose access is a RoleBinding to one customer's namespace.
// Every cluster-wide list refuses them, and telling them "your groups do not
// permit reading these" sends them to ask for access they already have. The
// console has to ask WHICH CUSTOMER instead.
//
// This was wired into the overview and nowhere else, so the rail — which is how
// most people leave the front page — led straight back to the refusal.
func TestARefusalThatIsReallyAQuestionIsAsked(t *testing.T) {
	forbidden := apierrors.NewForbidden(
		schema.GroupResource{Group: "doblura.dev", Resource: "odooenvironments"},
		"", errors.New("cluster-scoped list"))

	for _, c := range []struct {
		name  string
		view  objectsView
		scope string
		want  bool
	}{
		{"refused with no customer chosen: ask which",
			objectsView{Denied: true, refusal: forbidden}, "", true},
		{"refused with a customer already chosen: they really may not",
			objectsView{Denied: true, refusal: forbidden}, "demo", false},
		{"not refused at all",
			objectsView{}, "", false},
		// The cluster being down is not a permissions answer, and offering
		// "choose a customer" for it sends somebody to fix the wrong thing.
		{"unreachable, not refused",
			objectsView{Denied: true, refusal: errors.New("connection refused")}, "", false},
	} {
		r := httptest.NewRequest(http.MethodGet, "/o/environments", nil)
		if c.scope != "" {
			r.AddCookie(&http.Cookie{Name: scopeCookie, Value: c.scope})
		}
		if got := c.view.shouldAskForScope(r); got != c.want {
			t.Errorf("%s: asked=%v, want %v", c.name, got, c.want)
		}
	}
}
