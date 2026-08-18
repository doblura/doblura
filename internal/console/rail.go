// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Antoni Romera

package console

import "net/http"

// Whether the rail is collapsed.
//
// This was a checkbox and sibling selectors — no server, no cookie, no script.
// It also forgot the moment you followed a link, which is every interaction:
// somebody collapses the rail to get room, clicks a customer, and it is back.
// A preference that resets on navigation is worse than not having one.
//
// So it is a cookie set by a link, and the class is rendered by the server. Still
// no JavaScript, which is the property worth keeping on a page holding a session
// that can act on a cluster. The cost is one redirect when it is toggled.
const railCookie = "doblura_rail"

func railCollapsed(r *http.Request) bool {
	c, err := r.Cookie(railCookie)
	return err == nil && c.Value == "collapsed"
}

// handleRail records the choice and returns to where you were.
func (s *Server) handleRail(w http.ResponseWriter, r *http.Request, _ Identity) {
	value := "open"
	if r.URL.Query().Get("to") == "collapsed" {
		value = "collapsed"
	}
	http.SetCookie(w, &http.Cookie{
		Name: railCookie, Value: value, Path: "/", HttpOnly: true,
		Secure: r.TLS != nil, SameSite: http.SameSiteLaxMode, MaxAge: 365 * 24 * 3600,
	})

	// Same-site paths only. An absolute URL from a query string would make this
	// an open redirector, and it is a link somebody clicks without reading.
	back := r.URL.Query().Get("back")
	if back == "" || back[0] != '/' || (len(back) > 1 && back[1] == '/') {
		back = "/"
	}
	http.Redirect(w, r, back, http.StatusSeeOther)
}
