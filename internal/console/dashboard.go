// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Antoni Romera

package console

import (
	"net/http"
	"sort"
	"time"

	appsv1 "k8s.io/api/apps/v1"

	doblurav1alpha1 "github.com/doblura/doblura/api/v1alpha1"
)

// The landing page.
//
// It used to be the customer list, which is a fine page and the wrong first one:
// a list sorted by name asks the reader to scan it and decide what matters. The
// question somebody opens this with is "is anything wrong", and a page that
// answers it should say so before it says anything else.
//
// So the order is: what needs attention, then what is about to disappear, then
// the totals. When nothing needs attention it says that in a sentence rather
// than showing an empty table, because an empty table reads like a page that
// failed to load.

type dashboardView struct {
	// Attention is everything not in a good state, most severe first.
	Attention []attentionRow
	// Expiring are throwaway environments close to removing themselves. People
	// lose work to this, and a day's warning costs nothing.
	Expiring []attentionRow

	Customers    int
	Environments int
	Up           int
	// WorkloadVisible is false when the person cannot read Deployments, which
	// makes every "up" count meaningless rather than zero.
	WorkloadVisible bool

	Quotas []quotaRow
}

type attentionRow struct {
	Name     string
	Customer string
	Href     string
	State    string
	Word     string
	Detail   string
	severity int
}

type quotaRow struct {
	Customer string
	Href     string
	Open     int32
	Quota    int32
	Full     bool
}

func (s *Server) handleDashboard(w http.ResponseWriter, r *http.Request, id Identity) {
	c, err := s.clientFor(id)
	if err != nil {
		s.fail(w, id, err)
		return
	}
	ctx := r.Context()

	scope := scopeOption(r)

	var tenants doblurav1alpha1.OdooTenantList
	if err := c.List(ctx, &tenants, scope); err != nil {
		// A namespace-scoped person cannot make a cluster-wide list at all, and
		// their permissions are not the problem. Ask which customer instead of
		// showing them a refusal they cannot act on.
		if clusterWideRefusal(r, err) {
			s.askForScope(w, r, id, err)
			return
		}
		s.fail(w, id, err)
		return
	}
	var envs doblurav1alpha1.OdooEnvironmentList
	_ = c.List(ctx, &envs, scope)
	var deps appsv1.DeploymentList
	visible := c.List(ctx, &deps, scope) == nil

	view := dashboardView{
		Customers:       len(tenants.Items),
		Environments:    len(envs.Items),
		WorkloadVisible: visible,
	}

	displayName := map[string]string{}
	for i := range tenants.Items {
		t := &tenants.Items[i]
		displayName[t.Namespace+"/"+t.Name] = t.Spec.DisplayName
		quota := t.Spec.EphemeralQuota()
		view.Quotas = append(view.Quotas, quotaRow{
			Customer: t.Spec.DisplayName,
			Href:     "/c/" + t.Namespace + "/" + t.Name,
			Open:     t.Status.EphemeralEnvironments,
			Quota:    quota,
			Full:     t.Status.EphemeralEnvironments >= quota,
		})
	}

	for i := range envs.Items {
		e := &envs.Items[i]
		replicas, ready := replicasFor(&deps, e.Name)
		h := environmentHealth(e, replicas, ready, visible)
		if h.State == "up" {
			view.Up++
		}

		customer := displayName[e.Namespace+"/"+e.Spec.ForTenant]
		if customer == "" {
			customer = e.Spec.ForTenant
		}
		row := attentionRow{
			Name: e.Name, Customer: customer, State: h.State,
			Word: stateWord(h.State), Detail: h.Detail,
			Href: "/e/" + e.Namespace + "/" + e.Name,
		}

		switch h.State {
		case "down":
			row.severity = 0
			view.Attention = append(view.Attention, row)
		case "degraded":
			row.severity = 1
			view.Attention = append(view.Attention, row)
		case "building":
			// Not attention. Something being built is the system working, and
			// putting it in the same list as an outage teaches people to ignore
			// the list.
		}

		if left, ok := timeLeft(e); ok && left < expiryWarning {
			view.Expiring = append(view.Expiring, attentionRow{
				Name: e.Name, Customer: customer, Href: row.Href,
				State: "asleep", Word: "in " + shortDuration(left),
				Detail: "Removes itself when its time is up. Nothing is kept.",
			})
		}
	}

	sort.SliceStable(view.Attention, func(i, j int) bool {
		return view.Attention[i].severity < view.Attention[j].severity
	})
	sort.Slice(view.Quotas, func(i, j int) bool {
		if view.Quotas[i].Full != view.Quotas[j].Full {
			return view.Quotas[i].Full
		}
		return view.Quotas[i].Customer < view.Quotas[j].Customer
	})

	s.renderFor(w, r, "dashboard.html", page{
		Title: "Overview", Identity: id, Data: view,
	})
}

// expiryWarning is how far ahead the landing page warns.
//
// A day, because the thing being protected is somebody's unfinished work in a
// throwaway environment, and the remedy — copy it somewhere, or extend the ttl —
// takes minutes. An hour would be a warning nobody sees in time.
const expiryWarning = 24 * time.Hour

// timeLeft is how long an ephemeral environment has, and whether the question
// even applies.
//
// It checks the lifecycle TYPE and not the presence of a ttl, because ttl has a
// schema default and is never nil — testing it once told every persistent
// staging environment that it was about to delete itself.
func timeLeft(e *doblurav1alpha1.OdooEnvironment) (time.Duration, bool) {
	if e.Spec.Lifecycle.Type != doblurav1alpha1.LifecycleEphemeral {
		return 0, false
	}
	if e.Spec.Lifecycle.TTL == nil || e.Status.ReadyAt == nil {
		return 0, false
	}
	left := time.Until(e.Status.ReadyAt.Time.Add(e.Spec.Lifecycle.TTL.Duration))
	if left < 0 {
		left = 0
	}
	return left, true
}
