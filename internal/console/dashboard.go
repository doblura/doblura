// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Antoni Romera

package console

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"

	"sigs.k8s.io/controller-runtime/pkg/client"

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
	// All is every environment, so the page has something on it when nothing is
	// wrong — which is most days, and was the state that made it look broken.
	All []attentionRow

	Customers    int
	Environments int
	Up           int
	// WorkloadVisible is false when the person cannot read Deployments, which
	// makes every "up" count meaningless rather than zero.
	WorkloadVisible bool

	Quotas []quotaRow

	// Cluster is which one these came from, when they came from one.
	Cluster string
	// Everywhere means these numbers are the sum across every cluster, and each
	// row carries where it is.
	Everywhere bool
	// Troubles are the clusters that could not be asked. Named, never dropped:
	// a total that silently omits a cluster is a total somebody trusts.
	Troubles []clusterTrouble
}

type attentionRow struct {
	Name     string
	Customer string
	Cluster  string
	Href     string
	// What it is — "Demo data, persistent" — as opposed to how it is doing.
	//
	// On a list where every card already carries a green Up pill, repeating
	// "Answering normally" under each one is noise. What somebody scanning the
	// page cannot see from the colour is which of these is staging with real
	// data and which is a throwaway.
	What     string
	State    string
	Word     string
	Detail   string
	severity int
}

type quotaRow struct {
	Customer string
	Cluster  string
	Href     string
	Open     int32
	Quota    int32
	Full     bool
}

func (s *Server) handleDashboard(w http.ResponseWriter, r *http.Request, id Identity) {
	// Every cluster at once, when that is what was chosen. The counts add up and
	// the rows say where they are; a cluster that could not be asked is named
	// above them rather than quietly left out of the total.
	if s.Everywhere(r) {
		results := fanOut(r.Context(), s, id,
			func(ctx context.Context, who Identity) (dashboardView, error) {
				return s.dashboardIn(ctx, who, scopeOption(r))
			})
		s.renderFor(w, r, "dashboard.html", page{
			Title: "Overview", Identity: id, Data: mergeDashboards(results),
		})
		return
	}

	view, err := s.dashboardIn(r.Context(), id, scopeOption(r))
	if err != nil {
		// A namespace-scoped person cannot make a cluster-wide list at all, and
		// their permissions are not the problem. Ask which customer instead of
		// showing them a refusal they cannot act on.
		//
		// Lost once already: extracting dashboardIn moved the List out of the
		// handler and took this with it, which turned the fix into a 403 page for
		// exactly the people it was written for.
		if clusterWideRefusal(r, err) {
			s.askForScope(w, r, id, err)
			return
		}
		s.fail(w, id, err)
		return
	}
	s.renderFor(w, r, "dashboard.html", page{Title: "Overview", Identity: id, Data: view})
}

// dashboardIn is one cluster's overview.
//
// Split out so the same code answers one cluster and all of them: a second
// implementation for the aggregated view would drift, and the drift shows as two
// pages disagreeing about the same environment.
func (s *Server) dashboardIn(
	ctx context.Context,
	id Identity,
	scope client.ListOption,
) (dashboardView, error) {
	c, err := s.clientFor(id)
	if err != nil {
		return dashboardView{}, err
	}

	var tenants doblurav1alpha1.OdooTenantList
	if err := c.List(ctx, &tenants, scope); err != nil {
		return dashboardView{}, err
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
		if t.Status.EphemeralEnvironments < quota {
			// Only customers with no room left. A bar for everybody made the most
			// dominant thing on the overview a table about a normal state.
			continue
		}
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
			What: fmt.Sprintf("%s data, %s", e.Spec.Data.Type,
				strings.ToLower(string(e.Spec.Lifecycle.Type))),
			Href: "/e/" + e.Namespace + "/" + e.Name,
		}
		// Every environment, whatever state it is in.
		//
		// The overview used to show three enormous numbers and, when nothing was
		// wrong, nothing else: two thirds of the screen was white, which reads as
		// a page that failed to load rather than as a calm morning. The thing
		// somebody came to see is the environments, so they are on it.
		view.All = append(view.All, row)

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

	view.Cluster = id.Cluster
	return view, nil
}

// mergeDashboards adds up what every cluster answered.
func mergeDashboards(results []clusterResult[dashboardView]) dashboardView {
	out := dashboardView{Everywhere: true, Troubles: troubles(results), WorkloadVisible: true}
	for _, res := range results {
		if res.Err != nil {
			continue
		}
		v := res.Value
		out.Customers += v.Customers
		out.Environments += v.Environments
		out.Up += v.Up
		// One cluster whose workloads cannot be read makes the "up" count
		// meaningless everywhere, not partly meaningless: a total that counts
		// four clusters and guesses the fifth is worse than one that says it
		// cannot tell.
		if !v.WorkloadVisible {
			out.WorkloadVisible = false
		}
		out.Attention = append(out.Attention, stamped(v.Attention, res.Cluster)...)
		out.All = append(out.All, stamped(v.All, res.Cluster)...)
		out.Expiring = append(out.Expiring, stamped(v.Expiring, res.Cluster)...)
		for _, q := range v.Quotas {
			q.Cluster = res.Cluster
			q.Href = crossCluster(res.Cluster, q.Href)
			out.Quotas = append(out.Quotas, q)
		}
	}
	// Most severe first, as within one cluster: somebody scanning this list is
	// looking for what is broken, and interleaving by cluster would bury it.
	sort.SliceStable(out.Attention, func(i, j int) bool {
		return out.Attention[i].severity < out.Attention[j].severity
	})
	sort.Slice(out.All, func(i, j int) bool {
		if out.All[i].Customer != out.All[j].Customer {
			return out.All[i].Customer < out.All[j].Customer
		}
		return out.All[i].Name < out.All[j].Name
	})
	sort.Slice(out.Quotas, func(i, j int) bool {
		if out.Quotas[i].Cluster != out.Quotas[j].Cluster {
			return out.Quotas[i].Cluster < out.Quotas[j].Cluster
		}
		return out.Quotas[i].Customer < out.Quotas[j].Customer
	})
	return out
}

// stamped records which cluster each row came from and points its link there.
func stamped(rows []attentionRow, cluster string) []attentionRow {
	out := make([]attentionRow, 0, len(rows))
	for _, row := range rows {
		row.Cluster = cluster
		row.Href = crossCluster(cluster, row.Href)
		out = append(out, row)
	}
	return out
}

// crossCluster makes a link that switches cluster on the way.
//
// Through /cluster rather than a cluster in the path, so the choice is remembered
// and the back button behaves — and so every page about one object goes on being
// about one cluster, which is the only thing they can be.
func crossCluster(cluster, href string) string {
	if href == "" {
		return ""
	}
	return "/cluster?to=" + url.QueryEscape(cluster) + "&back=" + url.QueryEscape(href)
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
