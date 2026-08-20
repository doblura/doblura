// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Antoni Romera

package console

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/bcrypt"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

// Local accounts.
//
// This exists so the console is usable before anybody has an identity provider,
// and it is deliberately the SAME mechanism as OIDC from the RBAC side: a local
// account resolves to a username and a list of groups, and everything after that
// is impersonation and Kubernetes RBAC exactly as before. Moving a team from
// local accounts to SSO later changes who issues the identity and nothing else —
// no RoleBinding is rewritten, because the group names are yours either way.
//
// The accounts live in a Secret, one key per user, holding a bcrypt hash and the
// groups:
//
//	kubectl -n doblura-system create secret generic doblura-console-users \
//	  --from-literal=ana='$2a$12$...:doblura-support,doblura-qa'
//
// Two things about that which are worth saying out loud rather than discovering:
//
//   - Whoever can write this Secret can grant themselves any group, and therefore
//     any access the cluster binds to that group. It is a credential store with
//     the authority of a group membership store. Restrict it like one, and prefer
//     OIDC once you have it.
//   - The console reads the Secret with its OWN ServiceAccount, which is the one
//     place in this package where it uses a permission of its own rather than the
//     person's. It cannot be otherwise: there is no person yet at the moment the
//     password is checked.
type localAccounts struct {
	c         client.Client
	namespace string
	name      string

	mu      sync.RWMutex
	cached  map[string]localAccount
	fetched time.Time
}

type localAccount struct {
	Hash   []byte
	Groups []string
}

// accountTTL bounds how stale a password change or a group change can be.
//
// Short, because the failure it prevents is "I revoked their access and they
// were still in ten minutes later". Not zero, because a login page that reads a
// Secret on every keystroke of a brute-force attempt is a denial of service
// against the API server.
const accountTTL = 30 * time.Second

func (l *localAccounts) load(ctx context.Context) (map[string]localAccount, error) {
	l.mu.RLock()
	if l.cached != nil && time.Since(l.fetched) < accountTTL {
		defer l.mu.RUnlock()
		return l.cached, nil
	}
	l.mu.RUnlock()

	var sec corev1.Secret
	if err := l.c.Get(ctx, client.ObjectKey{Namespace: l.namespace, Name: l.name}, &sec); err != nil {
		if meta.IsNoMatchError(err) {
			return nil, err
		}
		return nil, fmt.Errorf("reading the accounts Secret %s/%s: %w", l.namespace, l.name, err)
	}

	accounts := make(map[string]localAccount, len(sec.Data))
	for user, raw := range sec.Data {
		hash, groups, found := strings.Cut(string(raw), ":")
		acc := localAccount{Hash: []byte(strings.TrimSpace(hash))}
		if found && strings.TrimSpace(groups) != "" {
			for _, g := range strings.Split(groups, ",") {
				if g = strings.TrimSpace(g); g != "" {
					acc.Groups = append(acc.Groups, g)
				}
			}
			sort.Strings(acc.Groups)
		}
		accounts[user] = acc
	}

	l.mu.Lock()
	l.cached, l.fetched = accounts, time.Now()
	l.mu.Unlock()
	return accounts, nil
}

var errBadCredentials = errors.New("that username and password do not match")

// authenticate checks a password.
//
// It compares against a dummy hash when the user does not exist, so a missing
// account and a wrong password take the same time. Without that, the login form
// is a user-enumeration oracle: bcrypt is slow by design, and "instant no"
// versus "200ms no" is a plainly readable difference.
func (l *localAccounts) authenticate(ctx context.Context, user, password string) (Identity, error) {
	accounts, err := l.load(ctx)
	if err != nil {
		return Identity{}, err
	}
	acc, ok := accounts[user]
	if !ok {
		_ = bcrypt.CompareHashAndPassword(dummyHash, []byte(password))
		return Identity{}, errBadCredentials
	}
	if err := bcrypt.CompareHashAndPassword(acc.Hash, []byte(password)); err != nil {
		return Identity{}, errBadCredentials
	}
	return Identity{User: user, Name: user, Groups: acc.Groups}, nil
}

// dummyHash is a real bcrypt hash of a value nobody will guess, at the same cost
// factor the accounts use, so the comparison above takes a realistic amount of
// time rather than returning immediately.
var dummyHash = []byte("$2a$12$C6UzMDM.H6dfI/f/IKcEe.6IQV6TPZbmZQZfnH8CS1UO.OQfoAy6i")

// ── the login form ──

func (s *Server) handleLoginForm(w http.ResponseWriter, r *http.Request) {
	s.render(w, "login.html", page{
		Title: "Sign in", Error: r.URL.Query().Get("e"),
		// Where they were going before the session ended, carried through the form
		// so signing in returns them there instead of to the overview.
		Back: safeNext(r.URL.Query().Get("next")),
	})
}

func (s *Server) handleLoginSubmit(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	id, err := s.local.authenticate(r.Context(), r.FormValue("user"), r.FormValue("password"))
	if err != nil {
		// The same message for a missing user and a wrong password, matching the
		// same-time comparison above: telling them which one they got right is
		// half the work of an attack.
		msg := errBadCredentials.Error()
		// But NOT for a failure to read the accounts at all. That used to fall in
		// here too, so a console whose ServiceAccount could not read its own
		// accounts Secret told everybody their password was wrong — a sentence
		// that sends people to reset a password that was always right, and hides
		// the one fact that would have fixed it. The person cannot act on the
		// detail, so the detail goes to the log and they get a true sentence.
		if !errors.Is(err, errBadCredentials) {
			log.Log.Error(err, "the console could not read its local accounts")
			msg = "sign-in is unavailable: the console cannot read its accounts. Ask whoever runs it to check its logs"
		}
		http.Redirect(w, r, "/auth/login?e="+url.QueryEscape(msg)+
			"&next="+url.QueryEscape(safeNext(r.FormValue("next"))), http.StatusSeeOther)
		return
	}
	s.setSession(w, r, id)
	http.Redirect(w, r, safeNext(r.FormValue("next")), http.StatusSeeOther)
}

// usernames is who has a local account.
//
// Names only, never the hashes: this feeds a page, and a bcrypt hash on a screen
// is a hash in a screenshot in a chat. Sorted, because the map is not and a list
// that reorders on every refresh reads as if it were changing.
func (l *localAccounts) usernames(ctx context.Context) ([]string, error) {
	accounts, err := l.load(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(accounts))
	for name := range accounts {
		out = append(out, name)
	}
	sort.Strings(out)
	return out, nil
}

// groupsOf is the groups a local account resolves to, for the same page.
func (l *localAccounts) groupsOf(ctx context.Context, user string) ([]string, error) {
	accounts, err := l.load(ctx)
	if err != nil {
		return nil, err
	}
	a, ok := accounts[user]
	if !ok {
		return nil, fmt.Errorf("no local account named %q", user)
	}
	return a.Groups, nil
}
