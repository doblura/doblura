// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Antoni Romera

package console

import (
	"net/http"
	"regexp"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Looking at one customer instead of all of them.
//
// The operator pages list cluster-wide, which is right for the people they were
// written for and wrong for everybody else. A list with no namespace is a
// CLUSTER-scoped read in Kubernetes, and a RoleBinding to one namespace does not
// permit it — so somebody granted viewer for a single customer, which is the
// documented way to grant it, opened the console and got a refusal on every page
// that lists anything. Their permissions were correct and the console could not
// use them.
//
// The fix is a scope, and it is a COOKIE rather than a path segment for the reason
// the collapsed rail is: it has to survive following a link. A scope in the URL
// would mean every link in the rail, every breadcrumb and every table cell had to
// carry it, and the first one that forgot would drop the person back onto the
// refusal they just escaped. One cookie, applied by every list, and navigation
// stays where it was.
//
// It is not a permission and cannot be one. Setting it narrows what this console
// ASKS for; what it may have is still decided by the API server on every request.
// Somebody who sets it to a customer they cannot read gets that customer's refusal,
// which is the correct answer and is also how they find out.

const scopeCookie = "doblura_scope"

// scopeNamePattern is what a namespace can be. Validated before it goes into a
// cookie and back out into a request: it arrives from a form, and a value that
// reaches an API call unchecked is a value worth checking.
var scopeNamePattern = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$`)

// scopeOf is the customer this request is looking at, or "" for all of them.
func scopeOf(r *http.Request) string {
	c, err := r.Cookie(scopeCookie)
	if err != nil || !scopeNamePattern.MatchString(c.Value) {
		return ""
	}
	return c.Value
}

// scopeOption turns it into something a List takes.
//
// InNamespace("") is exactly a cluster-wide list, so there is no branch here and
// no page that has to remember which shape it is in.
func scopeOption(r *http.Request) client.ListOption {
	return client.InNamespace(scopeOf(r))
}

// handleScope records the choice and returns to where you were.
func (s *Server) handleScope(w http.ResponseWriter, r *http.Request, _ Identity) {
	to := r.FormValue("to")
	if to == "" {
		to = r.URL.Query().Get("to")
	}

	cookie := &http.Cookie{
		Name: scopeCookie, Value: to, Path: "/", HttpOnly: true,
		Secure: r.TLS != nil, SameSite: http.SameSiteLaxMode, MaxAge: 30 * 24 * 3600,
	}
	if to == "" || !scopeNamePattern.MatchString(to) {
		// Clearing it, or refusing a value that is not a namespace name. Both end
		// up the same way: back to everything the person can see, which is the
		// state that needs no explanation.
		cookie.Value, cookie.MaxAge = "", -1
	}
	http.SetCookie(w, cookie)

	// Same-site paths only, like the rail: an absolute URL from a query string
	// would make this an open redirector, and it is a link somebody clicks
	// without reading.
	back := r.FormValue("back")
	if back == "" {
		back = r.URL.Query().Get("back")
	}
	if back == "" || back[0] != '/' || (len(back) > 1 && back[1] == '/') {
		back = "/"
	}
	http.Redirect(w, r, back, http.StatusSeeOther)
}

// clusterWideRefusal reports whether this error is the one a namespace-scoped
// person gets from a cluster-wide list, with no scope set to narrow it.
//
// Only a Forbidden counts. A page that offered "pick a customer" for a timeout or
// a broken connection would be telling somebody to fix the wrong thing.
func clusterWideRefusal(r *http.Request, err error) bool {
	return err != nil && apierrors.IsForbidden(err) && scopeOf(r) == ""
}

// scopeAsk is the page shown instead of that refusal.
type scopeAsk struct {
	// Refusal is the API server's own words, kept because it names the verb, the
	// resource and the person — which is what somebody forwards if the scope turns
	// out not to be the problem.
	Refusal string
	// Back is where to return once a scope is chosen.
	Back string
}

// askForScope renders it.
func (s *Server) askForScope(w http.ResponseWriter, r *http.Request, id Identity, err error) {
	s.renderFor(w, r, "scope.html", page{
		Title:    "Which customer?",
		Identity: id,
		Data:     scopeAsk{Refusal: err.Error(), Back: r.URL.Path},
	})
}
