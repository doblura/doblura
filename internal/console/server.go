// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Antoni Romera

package console

import (
	"context"
	"embed"
	"fmt"
	"encoding/json"
	"errors"
	"html/template"
	"net/http"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

// docsBase is where the published documentation lives.
//
// Not configurable, on purpose: it is the documentation for the version of the
// operator this console ships inside, and a per-install override would mostly be
// used to point somewhere stale.
const docsBase = "https://doblura.dev"

//go:embed assets/*
var assets embed.FS

//go:embed templates/*.html
var templates embed.FS

// Options configures the console.
type Options struct {
	Addr string

	// SessionKey signs the session cookie. Required, and required to be shared
	// by every replica: two replicas with different keys means every other
	// request logs the person out.
	SessionKey []byte

	// OIDC. When Issuer is empty the console refuses to start unless DevIdentity
	// is set — there is no third mode where it serves anonymous traffic.
	Issuer, ClientID, ClientSecret, RedirectURL string
	// GroupsClaim is where the identity provider puts group membership. "groups"
	// is the convention; Azure AD and some others differ.
	GroupsClaim string

	// LocalAccountsSecret is the Secret holding local accounts, in the console's
	// own namespace. Set it and the console serves its own sign-in form.
	//
	// Local accounts and OIDC can both be on: a team starts with local accounts,
	// adds their identity provider later, and moves people across without a
	// flag day. Both produce the same thing — a username and a list of groups —
	// so no RoleBinding changes when the last local account is deleted.
	LocalAccountsSecret string
	// Namespace is where that Secret lives. Defaults to the pod's namespace.
	Namespace string

	// DevIdentity is "user:group,group" and bypasses OIDC entirely. Local only:
	// it is announced in the interface on every page, because a console that
	// looks real and authenticates nobody is worse than one that will not start.
	DevIdentity string
}

// Server is the console.
type Server struct {
	cfg    *rest.Config
	scheme *runtime.Scheme
	opt    Options
	tpl    *template.Template
	oidc   *oidcProvider
	local  *localAccounts
}

// New builds the console. It does not start listening.
func New(cfg *rest.Config, scheme *runtime.Scheme, opt Options) (*Server, error) {
	s := &Server{cfg: cfg, scheme: scheme, opt: opt}

	funcs := template.FuncMap{
		"since": func(t *metaTime) string { return humanSince(t) },
		"join":  strings.Join,
		// Takes any, not a string: the API's enums are named types, and
		// strings.ToLower on one is a template error at render time rather
		// than a compile error anywhere.
		"lower": func(v any) string { return strings.ToLower(fmt.Sprint(v)) },
		"icon":  icon,
		// stateWord is what the colour and the icon say in words. Every state in
		// this interface carries all three, so none of them is load-bearing
		// alone.
		"stateWord": stateWord,
		"can": func(perms map[string]bool, key string) bool {
			return perms[key]
		},
		// dict lets one nav template be reused instead of eight near-identical
		// blocks that drift the first time an icon or a class changes.
		"dict": func(pairs ...any) map[string]any {
			m := make(map[string]any, len(pairs)/2)
			for i := 0; i+1 < len(pairs); i += 2 {
				k, _ := pairs[i].(string)
				m[k] = pairs[i+1]
			}
			return m
		},
		// safe marks help text as the HTML it is.
		//
		// Only ever called with literals written in these templates — never with
		// anything from the cluster, a form, or an identity provider. That is the
		// whole rule, and it is why this is a one-line helper rather than
		// something that takes a variable.
		"safe": func(s string) template.HTML { return template.HTML(s) }, //nolint:gosec // literals only

		// logTail so the page states the number it actually fetched rather than
		// a number somebody typed into the prose and then changed in the code.
		"logTail": func() int { return logTail },

		// mib, because a size in bytes is a number people have to divide in
		// their heads before it means anything.
		"mib": func(n int64) string {
			switch {
			case n <= 0:
				return "—"
			case n < 1<<20:
				return fmt.Sprintf("%d KiB", n>>10)
			case n < 1<<30:
				return fmt.Sprintf("%d MiB", n>>20)
			default:
				return fmt.Sprintf("%.1f GiB", float64(n)/float64(int64(1)<<30))
			}
		},

		// docs builds a link into the documentation site.
		//
		// A helper rather than literal URLs in templates, so the host is in ONE
		// place: a console pointing at a documentation site that has moved is
		// worse than one that links nowhere, because it looks like it works.
		"docs": func(path string) string {
			if path == "" {
				return docsBase
			}
			return docsBase + "/" + strings.TrimPrefix(path, "/")
		},
	}
	tpl, err := template.New("").Funcs(funcs).ParseFS(templates, "templates/*.html")
	if err != nil {
		return nil, err
	}
	s.tpl = tpl

	if opt.LocalAccountsSecret != "" {
		c, err := client.New(cfg, client.Options{Scheme: scheme})
		if err != nil {
			return nil, err
		}
		s.local = &localAccounts{c: c, namespace: opt.Namespace, name: opt.LocalAccountsSecret}
	}
	if opt.Issuer != "" {
		p, err := newOIDC(context.Background(), opt)
		if err != nil {
			return nil, err
		}
		s.oidc = p
	}
	// Three ways in, and none is "anybody". A console that starts with no way to
	// establish who somebody is would serve the customer list to the internet,
	// so this is a refusal to start rather than a warning in a log nobody reads.
	if s.oidc == nil && s.local == nil && opt.DevIdentity == "" {
		return nil, errors.New("no way to authenticate anyone: configure an OIDC issuer, " +
			"a local accounts Secret, or --dev-identity for local work")
	}
	return s, nil
}

// setSession issues the signed cookie. Shared by the local form and the OIDC
// callback, so the two entry points cannot drift in how long a session lasts or
// what it carries.
func (s *Server) setSession(w http.ResponseWriter, r *http.Request, id Identity) {
	sess := session{
		User: id.User, Groups: id.Groups, Email: id.Email, Name: id.Name,
		Expires: time.Now().Add(sessionTTL).Unix(),
	}
	payload, _ := json.Marshal(sess)
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: s.sign(payload), Path: "/", HttpOnly: true,
		Secure: r.TLS != nil, SameSite: http.SameSiteLaxMode,
		Expires: time.Unix(sess.Expires, 0),
	})
}

// Handler wires the routes.
//
// Every route that reads or writes cluster state goes through authenticated(),
// which resolves the session into an Identity. There is no route that talks to
// the API server without one, because the client cannot be built without one.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	mux.Handle("GET /assets/", http.FileServer(http.FS(assets)))
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})

	switch {
	case s.local != nil:
		// The local form is the landing page for sign-in even when OIDC is also
		// configured: it carries the button that starts the OIDC flow, so there
		// is one place to arrive at and two ways to leave it.
		mux.HandleFunc("GET /auth/login", s.handleLoginForm)
		mux.HandleFunc("POST /auth/login", s.handleLoginSubmit)
		if s.oidc != nil {
			mux.HandleFunc("GET /auth/sso", s.handleLogin)
			mux.HandleFunc("GET /auth/callback", s.handleCallback)
		}
	case s.oidc != nil:
		mux.HandleFunc("GET /auth/login", s.handleLogin)
		mux.HandleFunc("GET /auth/callback", s.handleCallback)
	}
	mux.HandleFunc("GET /auth/logout", s.handleLogout)

	mux.HandleFunc("GET /{$}", s.authenticated(s.handleDashboard))
	mux.HandleFunc("GET /customers", s.authenticated(s.handleCustomers))
	mux.HandleFunc("GET /c/{ns}/{name}", s.authenticated(s.handleCustomer))
	mux.HandleFunc("GET /e/{ns}/{name}", s.authenticated(s.handleEnvironment))
	mux.HandleFunc("GET /e/{ns}/{name}/logs", s.authenticated(s.handleLogs))
	mux.HandleFunc("POST /e/{ns}/{name}/delete", s.authenticated(s.handleDeleteEnvironment))
	mux.HandleFunc("POST /e/{ns}/{name}/settings", s.authenticated(s.handleEnvironmentSettings))
	mux.HandleFunc("POST /c/{ns}/{name}/environments", s.authenticated(s.handleCreateEnvironment))
	mux.HandleFunc("POST /c/{ns}/{name}/repos", s.authenticated(s.handleAddRepo))
	mux.HandleFunc("POST /c/{ns}/{name}/repos/remove", s.authenticated(s.handleRemoveRepo))
	mux.HandleFunc("POST /c/{ns}/{name}/images", s.authenticated(s.handleAddImage))
	mux.HandleFunc("POST /c/{ns}/{name}/images/remove", s.authenticated(s.handleRemoveImage))
	mux.HandleFunc("POST /c/{ns}/{name}/images/promote", s.authenticated(s.handlePromoteImage))
	mux.HandleFunc("GET /me", s.authenticated(s.handleWhoami))
	mux.HandleFunc("GET /access", s.authenticated(s.handleAccess))
	mux.HandleFunc("POST /access/grant", s.authenticated(s.handleAccessGrant))
	mux.HandleFunc("POST /access/revoke", s.authenticated(s.handleAccessRevoke))
	mux.HandleFunc("GET /o/{kind}", s.authenticated(s.handleObjects))
	mux.HandleFunc("GET /rail", s.authenticated(s.handleRail))
	mux.HandleFunc("GET /b/{ns}/{name}", s.authenticated(s.handleBackup))
	mux.HandleFunc("POST /b/{ns}/{name}/restore", s.authenticated(s.handleRestore))
	mux.HandleFunc("GET /rs/{ns}/{name}", s.authenticated(s.handleReviewSet))
	mux.HandleFunc("POST /rs/{ns}/{name}/pause", s.authenticated(s.handleReviewSetPause))
	mux.HandleFunc("POST /c/{ns}/{name}/reviewsets", s.authenticated(s.handleCreateReviewSet))

	return withSecurityHeaders(mux)
}

// ListenAndServe runs it.
func (s *Server) ListenAndServe(ctx context.Context) error {
	srv := &http.Server{
		Addr:              s.opt.Addr,
		Handler:           s.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdown)
	}()
	log.Log.Info("console listening", "addr", s.opt.Addr, "auth", s.authMode())
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return err
	}
	return nil
}

func (s *Server) authMode() string {
	switch {
	case s.opt.DevIdentity != "":
		return "DEVELOPMENT — no authentication"
	case s.local != nil && s.oidc != nil:
		return "local accounts, and single sign-on via " + s.opt.Issuer
	case s.local != nil:
		return "local accounts"
	default:
		return "single sign-on via " + s.opt.Issuer
	}
}

// withSecurityHeaders is a small, honest set.
//
// The CSP has no 'unsafe-inline': the pages are server-rendered and the styles
// live in a file, so there is nothing to whitelist. That is a reason to keep the
// interface server-rendered rather than an accident of it.
func withSecurityHeaders(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Security-Policy",
			"default-src 'none'; style-src 'self'; img-src 'self' data:; form-action 'self'; frame-ancestors 'none'; base-uri 'none'")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		h.ServeHTTP(w, r)
	})
}
