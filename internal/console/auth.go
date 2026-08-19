// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Antoni Romera

package console

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"
)

const sessionCookie = "doblura_session"
const stateCookie = "doblura_state"
const sessionTTL = 12 * time.Hour

type oidcProvider struct {
	verifier *oidc.IDTokenVerifier
	oauth    *oauth2.Config
	claim    string
}

func newOIDC(ctx context.Context, opt Options) (*oidcProvider, error) {
	if opt.Issuer == "" || opt.ClientID == "" || opt.RedirectURL == "" {
		return nil, errors.New("the console needs an OIDC issuer, client id and redirect url " +
			"(or --dev-identity for local work): it will not serve unauthenticated traffic")
	}
	p, err := oidc.NewProvider(ctx, opt.Issuer)
	if err != nil {
		return nil, fmt.Errorf("discovering %s: %w", opt.Issuer, err)
	}
	claim := opt.GroupsClaim
	if claim == "" {
		claim = "groups"
	}
	return &oidcProvider{
		verifier: p.Verifier(&oidc.Config{ClientID: opt.ClientID}),
		oauth: &oauth2.Config{
			ClientID:     opt.ClientID,
			ClientSecret: opt.ClientSecret,
			Endpoint:     p.Endpoint(),
			RedirectURL:  opt.RedirectURL,
			Scopes:       []string{oidc.ScopeOpenID, "profile", "email", claim},
		},
		claim: claim,
	}, nil
}

// session is what the cookie carries. It holds the identity and nothing else:
// no permissions, no profile name, no cached answers. Everything about what this
// person may do is asked of the API server on each request, so a role revoked in
// the cluster takes effect immediately rather than at the next login.
type session struct {
	User    string   `json:"u"`
	Groups  []string `json:"g"`
	Email   string   `json:"e,omitempty"`
	Name    string   `json:"n,omitempty"`
	Expires int64    `json:"x"`
}

func (s *Server) sign(payload []byte) string {
	m := hmac.New(sha256.New, s.opt.SessionKey)
	m.Write(payload)
	return base64.RawURLEncoding.EncodeToString(payload) + "." +
		base64.RawURLEncoding.EncodeToString(m.Sum(nil))
}

func (s *Server) unsign(v string) ([]byte, error) {
	parts := strings.SplitN(v, ".", 2)
	if len(parts) != 2 {
		return nil, errors.New("malformed session")
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return nil, err
	}
	want, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil, err
	}
	m := hmac.New(sha256.New, s.opt.SessionKey)
	m.Write(payload)
	// Constant time: a byte-by-byte compare here leaks the signature through
	// timing, and the thing it protects is "which groups am I in", which is the
	// only input to every authorization decision in the system.
	if !hmac.Equal(m.Sum(nil), want) {
		return nil, errors.New("bad signature")
	}
	return payload, nil
}

// identity resolves the request into a person, or fails.
func (s *Server) identity(r *http.Request) (Identity, error) {
	if s.opt.DevIdentity != "" {
		return parseDevIdentity(s.opt.DevIdentity)
	}
	c, err := r.Cookie(sessionCookie)
	if err != nil {
		return Identity{}, err
	}
	payload, err := s.unsign(c.Value)
	if err != nil {
		return Identity{}, err
	}
	var sess session
	if err := json.Unmarshal(payload, &sess); err != nil {
		return Identity{}, err
	}
	if time.Now().Unix() > sess.Expires {
		return Identity{}, errors.New("session expired")
	}
	return Identity{User: sess.User, Groups: sess.Groups, Email: sess.Email, Name: sess.Name}, nil
}

// parseDevIdentity reads "alice:doblura-support,doblura-qa".
func parseDevIdentity(v string) (Identity, error) {
	user, groups, _ := strings.Cut(v, ":")
	if user == "" {
		return Identity{}, errors.New("--dev-identity must be user[:group,group]")
	}
	id := Identity{User: user, Name: user}
	if groups != "" {
		id.Groups = strings.Split(groups, ",")
	}
	return id, nil
}

// authenticated wraps a handler that needs a person.
//
// A bounce carries where they were going. Without it, a session that ends —
// because it expired, or because somebody redeployed the console underneath
// them — turns every link into a trip to the sign-in page and then to the
// overview, and the person ends up hunting for the page they had open. It reads
// as the console losing their place, which is exactly what it was doing.
func (s *Server) authenticated(h func(http.ResponseWriter, *http.Request, Identity)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, err := s.identity(r)
		if err == nil {
			// WHERE, not who. Set once here so no handler has to remember, and
			// so it cannot be passed wrongly at one of twenty-eight call sites.
			// It changes which API server is asked and never what the answer is
			// allowed to be — see Identity.Cluster.
			id.Cluster = s.clusterOf(r)
		}
		if err != nil {
			http.Redirect(w, r, "/auth/login?next="+url.QueryEscape(safeNext(r.URL.RequestURI())),
				http.StatusSeeOther)
			return
		}
		h(w, r, id)
	}
}

// safeNext keeps a return path that cannot leave this site.
//
// Same rule as the rail's back parameter, and for the same reason: a path taken
// from a URL and used in a redirect is an open redirector, and this one appears on
// the sign-in page, which is the single best place to put one.
func safeNext(p string) string {
	if p == "" || p[0] != '/' || (len(p) > 1 && p[1] == '/') {
		return "/"
	}
	// Never bounce back to the sign-in flow itself: it would loop.
	if strings.HasPrefix(p, "/auth/") {
		return "/"
	}
	return p
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	state := randomToken()
	http.SetCookie(w, &http.Cookie{
		Name: stateCookie, Value: state, Path: "/", HttpOnly: true,
		Secure: r.TLS != nil, SameSite: http.SameSiteLaxMode, MaxAge: 600,
	})
	http.Redirect(w, r, s.oidc.oauth.AuthCodeURL(state), http.StatusSeeOther)
}

func (s *Server) handleCallback(w http.ResponseWriter, r *http.Request) {
	// The state cookie is the CSRF defence on the login flow itself: without it
	// an attacker can complete a login in somebody else's browser as themselves.
	c, err := r.Cookie(stateCookie)
	if err != nil || c.Value == "" || c.Value != r.URL.Query().Get("state") {
		http.Error(w, "state mismatch", http.StatusBadRequest)
		return
	}
	http.SetCookie(w, &http.Cookie{Name: stateCookie, Path: "/", MaxAge: -1})

	tok, err := s.oidc.oauth.Exchange(r.Context(), r.URL.Query().Get("code"))
	if err != nil {
		http.Error(w, "token exchange failed", http.StatusBadGateway)
		return
	}
	raw, ok := tok.Extra("id_token").(string)
	if !ok {
		http.Error(w, "the provider returned no id_token", http.StatusBadGateway)
		return
	}
	idt, err := s.oidc.verifier.Verify(r.Context(), raw)
	if err != nil {
		http.Error(w, "id_token did not verify", http.StatusUnauthorized)
		return
	}

	var claims map[string]any
	if err := idt.Claims(&claims); err != nil {
		http.Error(w, "unreadable claims", http.StatusBadGateway)
		return
	}

	sess := session{
		User:    idt.Subject,
		Groups:  stringsFrom(claims[s.oidc.claim]),
		Expires: time.Now().Add(sessionTTL).Unix(),
	}
	// Prefer a human-readable subject when the provider gives one: the username
	// ends up in RoleBindings and in the API server's audit log, and a UUID there
	// is a support ticket every time somebody reads it.
	if e, ok := claims["email"].(string); ok && e != "" {
		sess.Email = e
		sess.User = e
	}
	if n, ok := claims["name"].(string); ok {
		sess.Name = n
	}

	s.setSession(w, r, Identity{
		User: sess.User, Groups: sess.Groups, Email: sess.Email, Name: sess.Name,
	})
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Path: "/", MaxAge: -1})
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func stringsFrom(v any) []string {
	raw, ok := v.([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(raw))
	for _, x := range raw {
		if s, ok := x.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

func randomToken() string {
	b := make([]byte, 24)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}
