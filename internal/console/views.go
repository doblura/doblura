// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Antoni Romera

package console

import (
	"fmt"
	"net/http"
	"sort"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	doblurav1alpha1 "github.com/doblura/doblura/api/v1alpha1"
)

type metaTime = metav1.Time

// page is what every template gets. The banner fields are on every page rather
// than on a landing screen, because the two things a person most needs to know —
// who the system thinks they are, and whether this console is authenticating
// anybody at all — are exactly the two nobody goes looking for.
type page struct {
	Title    string
	Identity Identity
	DevMode  bool
	AuthMode string
	Perms    map[string]bool
	Error    string
	// SSO says whether the sign-in page should offer the identity provider as
	// well as the local form.
	SSO  bool
	Data any
}

func (s *Server) render(w http.ResponseWriter, name string, p page) {
	p.DevMode = s.opt.DevIdentity != ""
	p.AuthMode = s.authMode()
	p.SSO = s.oidc != nil
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.tpl.ExecuteTemplate(w, name, p); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

// fail renders the error the API server actually gave.
//
// Forwarded rather than replaced with "something went wrong": a 403 from
// impersonated RBAC names the verb, the resource and the user, which is exactly
// what the person needs to take to whoever administers their groups. Swallowing
// it would turn a self-service answer into a support ticket.
func (s *Server) fail(w http.ResponseWriter, id Identity, err error) {
	s.render(w, "error.html", page{Title: "Not available", Identity: id, Error: err.Error()})
}

// ── the customer list, which is the landing view for everyone ──

type customerRow struct {
	Tenant       *doblurav1alpha1.OdooTenant
	Environments int
	Ready        int
}

func (s *Server) handleCustomers(w http.ResponseWriter, r *http.Request, id Identity) {
	c, err := s.clientFor(id)
	if err != nil {
		s.fail(w, id, err)
		return
	}
	var tenants doblurav1alpha1.OdooTenantList
	if err := c.List(r.Context(), &tenants); err != nil {
		s.fail(w, id, err)
		return
	}
	var envs doblurav1alpha1.OdooEnvironmentList
	// A viewer can read these; if they somehow cannot, the list still renders
	// with zero counts rather than failing the whole page over a column.
	_ = c.List(r.Context(), &envs)

	rows := make([]customerRow, 0, len(tenants.Items))
	for i := range tenants.Items {
		t := &tenants.Items[i]
		row := customerRow{Tenant: t}
		for j := range envs.Items {
			e := &envs.Items[j]
			if e.Namespace != t.Namespace || e.Spec.ForTenant != t.Name {
				continue
			}
			row.Environments++
			if e.Status.Phase == doblurav1alpha1.EnvReady {
				row.Ready++
			}
		}
		rows = append(rows, row)
	}
	sort.Slice(rows, func(i, j int) bool {
		return rows[i].Tenant.Name < rows[j].Tenant.Name
	})

	s.render(w, "customers.html", page{Title: "Customers", Identity: id, Data: rows})
}

// ── one customer ──

type customerView struct {
	Tenant       *doblurav1alpha1.OdooTenant
	Environments []doblurav1alpha1.OdooEnvironment
	Rehearsals   []doblurav1alpha1.OdooRehearsal
	Quota        int32
	Open         int32
}

func (s *Server) handleCustomer(w http.ResponseWriter, r *http.Request, id Identity) {
	ns, name := r.PathValue("ns"), r.PathValue("name")
	c, err := s.clientFor(id)
	if err != nil {
		s.fail(w, id, err)
		return
	}
	var t doblurav1alpha1.OdooTenant
	if err := c.Get(r.Context(), client.ObjectKey{Namespace: ns, Name: name}, &t); err != nil {
		s.fail(w, id, err)
		return
	}
	var envs doblurav1alpha1.OdooEnvironmentList
	_ = c.List(r.Context(), &envs, client.InNamespace(ns))
	var rehearsals doblurav1alpha1.OdooRehearsalList
	_ = c.List(r.Context(), &rehearsals, client.InNamespace(ns))

	view := customerView{
		Tenant: &t,
		Quota:  t.Spec.EphemeralQuota(),
		Open:   t.Status.EphemeralEnvironments,
	}
	for i := range envs.Items {
		if envs.Items[i].Spec.ForTenant == name {
			view.Environments = append(view.Environments, envs.Items[i])
		}
	}
	view.Rehearsals = rehearsals.Items

	perms, err := s.allowed(r.Context(), id,
		CanCreateEnvironment(ns), CanCreateRehearsal(ns), CanReadLogs(ns))
	if err != nil {
		s.fail(w, id, err)
		return
	}
	s.render(w, "customer.html", page{
		Title: t.Spec.DisplayName, Identity: id, Perms: perms, Data: view,
	})
}

// ── one environment ──

type environmentView struct {
	Env  *doblurav1alpha1.OdooEnvironment
	Keys []conditionRow
}

type conditionRow struct {
	Type, Status, Reason, Message, Age string
}

func (s *Server) handleEnvironment(w http.ResponseWriter, r *http.Request, id Identity) {
	ns, name := r.PathValue("ns"), r.PathValue("name")
	c, err := s.clientFor(id)
	if err != nil {
		s.fail(w, id, err)
		return
	}
	var env doblurav1alpha1.OdooEnvironment
	if err := c.Get(r.Context(), client.ObjectKey{Namespace: ns, Name: name}, &env); err != nil {
		s.fail(w, id, err)
		return
	}
	view := environmentView{Env: &env}
	for _, cond := range env.Status.Conditions {
		view.Keys = append(view.Keys, conditionRow{
			Type: cond.Type, Status: string(cond.Status), Reason: cond.Reason,
			Message: cond.Message, Age: humanSince(&cond.LastTransitionTime),
		})
	}
	perms, err := s.allowed(r.Context(), id,
		CanDeleteEnvironment(ns, name), CanApprove(ns, name), CanReadLogs(ns))
	if err != nil {
		s.fail(w, id, err)
		return
	}
	s.render(w, "environment.html", page{
		Title: env.Name, Identity: id, Perms: perms, Data: view,
	})
}

// ── the task launcher ──

func (s *Server) handleCreateEnvironment(w http.ResponseWriter, r *http.Request, id Identity) {
	ns, tenant := r.PathValue("ns"), r.PathValue("name")
	if err := r.ParseForm(); err != nil {
		s.fail(w, id, err)
		return
	}
	c, err := s.clientFor(id)
	if err != nil {
		s.fail(w, id, err)
		return
	}

	env := &doblurav1alpha1.OdooEnvironment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      r.FormValue("name"),
			Namespace: ns,
		},
		// Four fields, and every one of them is a decision this person is
		// qualified to make. The image, the database and the filestore are
		// filled by the mutating webhook from the customer record, because
		// support does not know the Postgres host and should not be asked.
		Spec: doblurav1alpha1.OdooEnvironmentSpec{
			ForTenant: tenant,
			Data:      doblurav1alpha1.EnvData{Type: doblurav1alpha1.EnvDataType(r.FormValue("data"))},
			Lifecycle: doblurav1alpha1.EnvLifecycle{Type: doblurav1alpha1.LifecycleEphemeral},
		},
	}
	// Created as the person, not as the console. If they are over quota the
	// admission webhook says so, in its own words, and that message goes
	// straight to the screen — the console does not need to know the rule.
	if err := c.Create(r.Context(), env); err != nil {
		s.fail(w, id, err)
		return
	}
	http.Redirect(w, r, fmt.Sprintf("/e/%s/%s", ns, env.Name), http.StatusSeeOther)
}

func (s *Server) handleDeleteEnvironment(w http.ResponseWriter, r *http.Request, id Identity) {
	ns, name := r.PathValue("ns"), r.PathValue("name")
	c, err := s.clientFor(id)
	if err != nil {
		s.fail(w, id, err)
		return
	}
	env := &doblurav1alpha1.OdooEnvironment{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
	}
	if err := c.Delete(r.Context(), env); err != nil {
		s.fail(w, id, err)
		return
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// ── who the system thinks you are ──

func (s *Server) handleWhoami(w http.ResponseWriter, r *http.Request, id Identity) {
	// Asked in the namespaces that exist rather than cluster-wide: a consultant
	// scoped to one customer with a RoleBinding is allowed there and nowhere
	// else, and a cluster-wide question would report a flat "no" and hide the
	// scoping that is the whole point of binding them that way.
	c, err := s.clientFor(id)
	if err != nil {
		s.fail(w, id, err)
		return
	}
	var tenants doblurav1alpha1.OdooTenantList
	_ = c.List(r.Context(), &tenants)

	type scope struct {
		Namespace string
		Perms     map[string]bool
	}
	seen := map[string]bool{}
	var scopes []scope
	for i := range tenants.Items {
		ns := tenants.Items[i].Namespace
		if seen[ns] {
			continue
		}
		seen[ns] = true
		perms, err := s.allowed(r.Context(), id,
			CanCreateEnvironment(ns), CanCreateRehearsal(ns), CanReadLogs(ns))
		if err != nil {
			continue
		}
		scopes = append(scopes, scope{Namespace: ns, Perms: perms})
	}
	s.render(w, "me.html", page{Title: "Your access", Identity: id, Data: scopes})
}

func humanSince(t *metav1.Time) string {
	if t == nil || t.IsZero() {
		return "—"
	}
	d := time.Since(t.Time).Round(time.Second)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
}
