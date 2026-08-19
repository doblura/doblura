// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Antoni Romera

package console

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"strings"

	rbacv1 "k8s.io/api/rbac/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Who has access, and how they get in.
//
// This page exists because the two questions it answers were unanswerable from
// the interface, and both of them are asked under pressure: somebody has left and
// nobody can say what they could still reach, or somebody new cannot get in and
// nobody can say where the identities come from. Both were answerable only with
// kubectl and a working idea of RBAC, which is the wrong bar for the person who
// administers a team.
//
// It is READ from the cluster, never tabulated in Go. The personas ship as
// ClusterRoles and are meant to be bound to the customer's own group names, so a
// hardcoded table here would be wrong for every real installation and right only
// for the demo. What is on screen is what the API server would enforce.
//
// And it is deliberately not a user database. Doblura has no users: an identity
// arrives from an identity provider or from the local-accounts Secret, and
// everything after that is a username and a list of groups. Presenting a "user
// list" would invent an authority this system does not have and should not have —
// the moment doblura owns identities, it owns password resets, lockouts and
// offboarding, and it would own them worse than the identity provider already
// does.

// accessGrant is one binding, flattened to something a person can read.
type accessGrant struct {
	// Subject is the person, group or service account the grant is for.
	Subject string
	// Kind is User, Group or ServiceAccount, which changes what it means: a
	// Group is the one that keeps working when somebody joins the team.
	Kind string
	// Persona is the role's name. Not prettified: it is what somebody has to
	// type to change it.
	Persona string
	// Scope is the namespace, or empty for the whole cluster. The difference is
	// the whole risk: the same persona bound cluster-wide reaches every customer.
	Scope string
	// Binding is the object to edit or delete, so the page can be acted on.
	Binding string
	// ClusterWide is Scope == "", broken out because templates cannot compare
	// against an empty string as legibly as this reads.
	ClusterWide bool
}

// accessView is the whole page.
type accessView struct {
	Grants []accessGrant
	// Personas are doblura's own roles, whether or not anybody is bound to
	// them, so an empty list reads as "nobody has this yet" rather than as
	// "this role does not exist".
	Personas []personaSummary
	// Login is how identities arrive.
	Login loginConfig
	// Unbound counts personas nobody holds, which is worth saying: a platform
	// with no platform persona bound is one nobody can administer.
	Unbound []string
	// CanGrant, CanRevoke and CanSeeAccounts are what this person may do here,
	// asked of the API server rather than assumed from their persona.
	CanGrant       bool
	CanRevoke      bool
	CanSeeAccounts bool

	// Denied is set when the person cannot read bindings. The page still renders
	// the login configuration, which needs no RBAC beyond being signed in.
	Denied string
}

type personaSummary struct {
	Name string
	// Summary is the role's own description, taken from an annotation on the
	// ClusterRole so it lives beside what it describes.
	Summary string
	// Holders is how many grants point at it.
	Holders int
	// Verbs is a short human reading of what it can do, derived from the rules.
	Can []string
}

// loginConfig is where identities come from, as configured — not as documented.
type loginConfig struct {
	// Mode is "sso", "local", "both" or "none".
	Mode string
	// SSO fields, empty when there is no provider.
	Issuer      string
	ClientID    string
	GroupsClaim string
	RedirectURL string
	// LocalSecret is the Secret holding local accounts, empty when unused.
	LocalSecret string
	// LocalUsers is who is in that Secret. Read only if the person can read it,
	// which is deliberately a high bar: it is a credential store.
	LocalUsers []string
	LocalError string
	// AccountsWithheld says the roster exists but this person may not read it,
	// which is a different statement from "there are none".
	AccountsWithheld bool
	// DevIdentity is set when the console is trusting a flag instead of
	// authenticating anybody, which must be impossible to miss.
	DevIdentity string
}

const (
	// personaLabel marks a ClusterRole as one of doblura's personas. Its value is
	// the persona's short name and is not read here: presence is the question.
	personaLabel = "doblura.dev/persona"
	// personaSummaryAnnotation is the one-line description, kept on the role
	// itself so it cannot drift from what the role actually grants.
	personaSummaryAnnotation = "doblura.dev/summary"
)

// handleAccess renders it.
func (s *Server) handleAccess(w http.ResponseWriter, r *http.Request, id Identity) {
	ctx := r.Context()
	view := accessView{Login: s.loginConfig(ctx, id)}

	c, err := s.clientFor(id)
	if err != nil {
		s.fail(w, id, err)
		return
	}

	// What this page may OFFER is asked of the API server, like everywhere else.
	// A grant form shown to somebody who cannot create a RoleBinding is a button
	// that produces a 403, and the person reasonably reads that as a bug.
	perms, permErr := s.allowed(ctx, id,
		Verb{"create", "rolebindings", "", ""},
		Verb{"delete", "rolebindings", "", ""},
		Verb{"get", "secrets", s.opt.Namespace, s.opt.LocalAccountsSecret},
	)
	if permErr != nil {
		perms = map[string]bool{}
	}
	view.CanGrant = perms[Verb{"create", "rolebindings", "", ""}.key()]
	view.CanRevoke = perms[Verb{"delete", "rolebindings", "", ""}.key()]
	view.CanSeeAccounts = perms[Verb{"get", "secrets", s.opt.Namespace, s.opt.LocalAccountsSecret}.key()]
	if !view.CanSeeAccounts {
		// The roster is withheld rather than the panel: somebody has to be able
		// to see that local accounts are ON without being able to read them.
		view.Login.LocalUsers = nil
		view.Login.LocalError = ""
		view.Login.AccountsWithheld = true
	}

	roles, grants, err := dobluraRBAC(ctx, c)
	if err != nil {
		// Not a failure page. Somebody who cannot read RoleBindings can still
		// need to know where sign-in comes from, and a 403 on one panel should
		// not take the other one down with it.
		view.Denied = err.Error()
		s.renderFor(w, r, "access.html", page{
			Title: "Access", Identity: id, Data: view,
		})
		return
	}

	view.Grants = grants
	held := map[string]int{}
	for _, g := range grants {
		held[g.Persona]++
	}
	for _, role := range roles {
		p := personaSummary{
			Name:    role.Name,
			Summary: role.Annotations[personaSummaryAnnotation],
			Holders: held[role.Name],
			Can:     summariseRules(role.Rules),
		}
		view.Personas = append(view.Personas, p)
		if p.Holders == 0 {
			view.Unbound = append(view.Unbound, role.Name)
		}
	}

	s.renderFor(w, r, "access.html", page{
		Title: "Access", Identity: id, Data: view,
	})
}

// dobluraRBAC finds doblura's personas and everything bound to them.
//
// A persona is identified by its label rather than by a name prefix: an
// administrator who copies a persona into their own naming scheme keeps the label
// and appears here, which is the behaviour somebody expects after copying it. The
// label is asked for by PRESENCE, not by value, so a persona somebody invents for
// their own organisation is listed beside the ones the chart ships.
func dobluraRBAC(ctx context.Context, c client.Client) ([]rbacv1.ClusterRole, []accessGrant, error) {
	var roles rbacv1.ClusterRoleList
	if err := c.List(ctx, &roles,
		client.HasLabels{personaLabel}); err != nil {
		return nil, nil, fmt.Errorf("listing the personas: %w", err)
	}

	ours := map[string]bool{}
	for i := range roles.Items {
		ours[roles.Items[i].Name] = true
	}
	sort.Slice(roles.Items, func(i, j int) bool {
		return roles.Items[i].Name < roles.Items[j].Name
	})

	var grants []accessGrant

	var crbs rbacv1.ClusterRoleBindingList
	if err := c.List(ctx, &crbs); err != nil {
		return nil, nil, fmt.Errorf("listing cluster-wide grants: %w", err)
	}
	for i := range crbs.Items {
		b := &crbs.Items[i]
		if !ours[b.RoleRef.Name] {
			continue
		}
		for _, sub := range b.Subjects {
			grants = append(grants, accessGrant{
				Subject: subjectName(sub), Kind: sub.Kind,
				Persona: b.RoleRef.Name, Binding: b.Name, ClusterWide: true,
			})
		}
	}

	var rbs rbacv1.RoleBindingList
	if err := c.List(ctx, &rbs); err != nil {
		return nil, nil, fmt.Errorf("listing per-customer grants: %w", err)
	}
	for i := range rbs.Items {
		b := &rbs.Items[i]
		// Only ClusterRole refs: a RoleBinding to a namespaced Role of the same
		// name is somebody else's role that happens to collide.
		if b.RoleRef.Kind != "ClusterRole" || !ours[b.RoleRef.Name] {
			continue
		}
		for _, sub := range b.Subjects {
			grants = append(grants, accessGrant{
				Subject: subjectName(sub), Kind: sub.Kind,
				Persona: b.RoleRef.Name, Scope: b.Namespace, Binding: b.Name,
			})
		}
	}

	// Cluster-wide first, then by subject: the grants that reach everything are
	// the ones worth reading first, and they are what an audit is looking for.
	sort.Slice(grants, func(i, j int) bool {
		if grants[i].ClusterWide != grants[j].ClusterWide {
			return grants[i].ClusterWide
		}
		if grants[i].Subject != grants[j].Subject {
			return grants[i].Subject < grants[j].Subject
		}
		return grants[i].Persona < grants[j].Persona
	})

	return roles.Items, grants, nil
}

// subjectName is how a subject is written, including the namespace a
// ServiceAccount needs to be identified at all.
func subjectName(sub rbacv1.Subject) string {
	if sub.Kind == "ServiceAccount" && sub.Namespace != "" {
		return sub.Namespace + "/" + sub.Name
	}
	return sub.Name
}

// summariseRules says what a persona can do, in words, from its own rules.
//
// Deliberately coarse. The point is not to reproduce the RBAC — the page links to
// the object for that — but to answer "is this the read-only one?" at a glance,
// which is the question somebody has when choosing which persona to grant.
func summariseRules(rules []rbacv1.PolicyRule) []string {
	writes := map[string]bool{}
	reads := map[string]bool{}
	for _, rule := range rules {
		verbs := map[string]bool{}
		for _, v := range rule.Verbs {
			verbs[v] = true
		}
		mutating := verbs["*"] || verbs["create"] || verbs["update"] ||
			verbs["patch"] || verbs["delete"]
		for _, res := range rule.Resources {
			// The subresource is what the parent grants, for this purpose.
			if i := strings.IndexByte(res, '/'); i >= 0 {
				res = res[:i]
			}
			// A wildcard is legal and reads as nothing. Said in words instead,
			// naming the group, because "changes *" on the persona that runs the
			// platform is exactly the line somebody needs to understand.
			if res == "*" {
				res = "everything in " + apiGroupName(rule.APIGroups)
			}
			if mutating {
				writes[res] = true
				continue
			}
			reads[res] = true
		}
	}

	var out []string
	if len(writes) > 0 {
		out = append(out, "changes "+strings.Join(sortedKeys(writes), ", "))
	}
	// Only mention reading what it cannot also change, or every line says both.
	readOnly := map[string]bool{}
	for k := range reads {
		if !writes[k] {
			readOnly[k] = true
		}
	}
	if len(readOnly) > 0 {
		out = append(out, "reads "+strings.Join(sortedKeys(readOnly), ", "))
	}
	if len(out) == 0 {
		out = append(out, "nothing: this persona grants no rules")
	}
	return out
}

// apiGroupName is how an API group is written to somebody reading a page.
func apiGroupName(groups []string) string {
	if len(groups) == 0 {
		return "Kubernetes"
	}
	switch groups[0] {
	case "":
		return "Kubernetes"
	case "doblura.dev":
		return "doblura"
	case "*":
		return "the whole cluster"
	default:
		return groups[0]
	}
}

func sortedKeys(in map[string]bool) []string {
	out := make([]string, 0, len(in))
	for k := range in {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// loginConfig reports how sign-in is actually wired.
//
// From the running configuration rather than from the chart's values, because the
// question being asked is "why can this person not get in", and values.yaml is
// what somebody INTENDED. A console started with an issuer it could not discover
// has no provider at runtime, and this says so.
func (s *Server) loginConfig(ctx context.Context, id Identity) loginConfig {
	cfg := loginConfig{DevIdentity: s.opt.DevIdentity}

	if s.oidc != nil {
		cfg.Issuer = s.opt.Issuer
		cfg.ClientID = s.opt.ClientID
		cfg.RedirectURL = s.opt.RedirectURL
		cfg.GroupsClaim = s.opt.GroupsClaim
		if cfg.GroupsClaim == "" {
			cfg.GroupsClaim = "groups"
		}
	}
	if s.local != nil {
		cfg.LocalSecret = s.opt.LocalAccountsSecret
		// Read with the console's own client, because the console must be able
		// to read this Secret to sign anybody in at all. Whether the PERSON gets
		// to see it is decided separately, by asking the API server whether they
		// could read it themselves — otherwise this page would leak the roster
		// to anybody who can open it.
		users, err := s.local.usernames(ctx)
		switch {
		case err != nil:
			cfg.LocalError = err.Error()
		default:
			cfg.LocalUsers = users
		}
	}

	switch {
	case cfg.Issuer != "" && cfg.LocalSecret != "":
		cfg.Mode = "both"
	case cfg.Issuer != "":
		cfg.Mode = "sso"
	case cfg.LocalSecret != "":
		cfg.Mode = "local"
	default:
		cfg.Mode = "none"
	}
	return cfg
}

// handleAccessGrant creates a grant, and handleAccessRevoke removes one.
//
// Both act AS the person through impersonation, so the answer to "may I" is the
// API server's and not this page's. That matters more here than anywhere else in
// the console: a page that can grant access is a privilege-escalation path if it
// holds any permission of its own, and Kubernetes already refuses to let somebody
// grant a permission they do not themselves hold.
func (s *Server) handleAccessGrant(w http.ResponseWriter, r *http.Request, id Identity) {
	ctx := r.Context()
	c, err := s.clientFor(id)
	if err != nil {
		s.fail(w, id, err)
		return
	}

	kind := r.FormValue("kind")
	subject := strings.TrimSpace(r.FormValue("subject"))
	persona := strings.TrimSpace(r.FormValue("persona"))
	scope := strings.TrimSpace(r.FormValue("scope"))

	if subject == "" || persona == "" {
		s.failBack(w, r, id, fmt.Errorf(
			"a grant needs somebody to grant it to and a persona to grant"))
		return
	}
	if kind != "User" && kind != "Group" {
		s.failBack(w, r, id, fmt.Errorf(
			"a grant is for a User or a Group. ServiceAccounts are not granted "+
				"here on purpose: a token that holds a persona is a credential "+
				"with no person attached, and this page would be the wrong place "+
				"to create one quietly"))
		return
	}

	// Refuse a cluster-wide grant from this page. It is the one grant that
	// reaches every customer at once, it is the one somebody makes by leaving a
	// field blank, and it is rare enough to be worth doing deliberately with
	// kubectl where it will be reviewed.
	if scope == "" {
		s.failBack(w, r, id, fmt.Errorf(
			"choose the customer this grant is for. A grant with no customer "+
				"reaches every customer in the cluster, and this page will not "+
				"make one: apply a ClusterRoleBinding directly if that is what "+
				"you mean"))
		return
	}

	name := grantName(kind, subject, persona)
	rb := &rbacv1.RoleBinding{}
	rb.Name = name
	rb.Namespace = scope
	rb.Labels = map[string]string{
		"app.kubernetes.io/part-of":    "doblura",
		"app.kubernetes.io/managed-by": "doblura-console",
	}
	// Recorded on the object, because "who granted this" is the question asked
	// afterwards and the API server's audit log is not somewhere most people can
	// look.
	rb.Annotations = map[string]string{"doblura.dev/granted-by": id.User}
	rb.RoleRef = rbacv1.RoleRef{
		APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: persona,
	}
	rb.Subjects = []rbacv1.Subject{{
		APIGroup: rbacv1.GroupName, Kind: kind, Name: subject,
	}}

	if err := c.Create(ctx, rb); err != nil {
		s.failBack(w, r, id, err)
		return
	}
	http.Redirect(w, r, "/access", http.StatusSeeOther)
}

func (s *Server) handleAccessRevoke(w http.ResponseWriter, r *http.Request, id Identity) {
	ctx := r.Context()
	c, err := s.clientFor(id)
	if err != nil {
		s.fail(w, id, err)
		return
	}

	name, scope := r.FormValue("binding"), r.FormValue("scope")
	if name == "" {
		s.failBack(w, r, id, fmt.Errorf("no grant was named"))
		return
	}

	if scope == "" {
		// Symmetrical with granting: this page does not remove the grants it
		// would not create. Revoking a cluster-wide binding through a form is
		// also how somebody locks the platform out of its own cluster.
		s.failBack(w, r, id, fmt.Errorf(
			"%s is a cluster-wide grant, and this page does not remove those: "+
				"kubectl delete clusterrolebinding %s. It is left deliberately "+
				"awkward because removing the wrong one locks everybody out",
			name, name))
		return
	}

	rb := &rbacv1.RoleBinding{}
	rb.Name = name
	rb.Namespace = scope
	if err := c.Delete(ctx, rb); err != nil {
		s.failBack(w, r, id, err)
		return
	}
	http.Redirect(w, r, "/access", http.StatusSeeOther)
}

// grantName is deterministic, so granting the same thing twice is a conflict the
// API server reports rather than a second identical binding nobody notices.
func grantName(kind, subject, persona string) string {
	return "doblura-" + strings.ToLower(kind) + "-" +
		dnsSafe(subject) + "-" + strings.TrimPrefix(persona, "doblura-")
}

// dnsSafe makes a subject usable in an object name. Identity providers hand out
// names with @ and : in them, which are legal identities and illegal names.
func dnsSafe(in string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(in) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	out := strings.Trim(b.String(), "-")
	// 63 is the label limit, and the persona suffix still has to fit.
	if len(out) > 40 {
		out = strings.Trim(out[:40], "-")
	}
	if out == "" {
		out = "subject"
	}
	return out
}
