// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Antoni Romera

package console

import "net/http"

// Light, dark, or whatever the machine says.
//
// The stylesheet has had `:root[data-theme="dark"]` and its light counterpart
// since it was written, and nothing ever set the attribute: only
// prefers-color-scheme worked, so somebody whose laptop is set to light and who
// wants this dark had no way to say so. Rules nothing can reach are the same shape
// as an Ingress naming a middleware nothing creates — the CSS was written against
// a mechanism that did not exist.
//
// A cookie and a server-rendered attribute, like the rail and the customer scope.
// No JavaScript, which is the property worth keeping on a page holding a session
// that can act on a cluster, and it means the right theme is in the first byte
// rather than applied after a flash of the wrong one.

const themeCookie = "doblura_theme"

// themeOf is the theme this request asked for, or "" for the machine's own.
func themeOf(r *http.Request) string {
	c, err := r.Cookie(themeCookie)
	if err != nil {
		return ""
	}
	switch c.Value {
	case "light", "dark":
		return c.Value
	default:
		// Anything else, including the "auto" this sets when cleared, means no
		// attribute and prefers-color-scheme decides.
		return ""
	}
}

// handleTheme records the choice and returns to where you were.
func (s *Server) handleTheme(w http.ResponseWriter, r *http.Request, _ Identity) {
	to := r.URL.Query().Get("to")
	cookie := &http.Cookie{
		Name: themeCookie, Value: to, Path: "/", HttpOnly: true,
		Secure: r.TLS != nil, SameSite: http.SameSiteLaxMode, MaxAge: 365 * 24 * 3600,
	}
	if to != "light" && to != "dark" {
		cookie.Value, cookie.MaxAge = "", -1
	}
	http.SetCookie(w, cookie)

	// Same-site paths only, like the rail: a redirect target from a query string
	// is an open redirector, and this is a link somebody clicks without reading.
	back := r.URL.Query().Get("back")
	if back == "" || back[0] != '/' || (len(back) > 1 && back[1] == '/') {
		back = "/"
	}
	http.Redirect(w, r, back, http.StatusSeeOther)
}
