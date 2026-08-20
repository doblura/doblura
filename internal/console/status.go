// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Antoni Romera

package console

import (
	"context"
	"fmt"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"net/http"
	"sort"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	doblurav1alpha1 "github.com/doblura/doblura/api/v1alpha1"
)

// The screen for the customer whose Odoo this is.
//
// Everything else in this console is for somebody who runs the platform. This one
// is for the person who phones them, and it answers the two questions that phone
// call is actually about: is it up, and is it slow. Nothing else. No object names
// they cannot act on, no logs, no other customers, no rail full of pages that
// would 403.
//
// Three things make it a different page rather than a filtered version of the
// dashboard:
//
//   - It is scoped by RBAC. The URL carries the customer's namespace, but it grants
//     nothing: the read is made AS the signed-in person, so a link to somebody
//     else's namespace returns that person's own 403 rather than their data. The
//     namespace is in the path because the alternative does not work — a list with
//     no namespace is a CLUSTER-scoped read, which a RoleBinding to one namespace
//     does not permit, and the customer persona is meant to be bound to one
//     namespace. The first version listed cluster-wide and every customer saw the
//     honest refusal instead of their own status.
//   - It renders its own layout with no navigation. A customer given the operator
//     rail would spend their time clicking into refusals, and every one of those
//     reads as the platform being broken.
//   - It says when it last looked and offers a reload, because "live" on a page
//     with no JavaScript means "as of a moment ago" and pretending otherwise is
//     how somebody stares at a stale screen during an outage.
//
// It is READ ONLY, deliberately. The customer persona can restart nothing: an
// environment restarted by the person reporting a problem, while somebody is
// looking into it, is worse than an environment nobody restarted.

// statusRow is one environment, in the words a customer uses.
type statusRow struct {
	Name     string
	Customer string
	// Purpose is shown only when there is more than one environment, because
	// "Production" beside a single row is noise and beside three rows is the
	// difference between panicking and not.
	Purpose string
	// State is the stylesheet's word: up, degraded, down, building, asleep.
	State string
	// Headline is what a person says out loud: "It is working".
	Headline string
	// Detail is one sentence more, written for a customer.
	//
	// NOT the health detail the rest of the console shows. That one is written for
	// whoever runs the platform and names the objects they would look at — the
	// first version of this page passed it straight through and told a customer to
	// "check the logs of Job prod-sinbackup-migrate", which names an object they
	// cannot see, asks for a permission they do not have, and reads as a second
	// fault. A customer does not act on a phase name; they act on knowing whether
	// it is them or us, and whether anybody knows.
	Detail string
	// Address is where to open it, when it is exposed.
	Address string

	// Load is present only when the cluster actually reports it.
	Load       string
	LoadDetail string
	LoadState  string

	// Running says there is something to measure. False when it is down or still
	// starting, so the page does not report the absence of a load figure as if it
	// were a second thing wrong.
	Running bool

	// Since is how long it has been in this state, when that is known.
	Since string
}

type statusView struct {
	Rows []statusRow
	// Locale is the language this customer's own people read, taken from the
	// customer record. See i18n.go for why it is not the reader's choice.
	Locale locale
	// LookedAt is when the page was rendered. Stated because a page with no
	// JavaScript is never live, and a status screen that implies it is will be
	// trusted at exactly the wrong moment.
	LookedAt string
	// Everything is true when every row is up, so the page can lead with one
	// sentence instead of making somebody read a table.
	Everything bool
	// Multiple is len(Rows) > 1.
	Multiple bool
	// Scope is the customer this page was asked about, empty when the whole
	// cluster was. Shown so a person sent a link knows whose status they are
	// looking at.
	Scope  string
	Denied string
}

// handleStatus renders it.
func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request, id Identity) {
	ctx := r.Context()
	view := statusView{LookedAt: time.Now().Format("15:04:05 MST")}

	c, err := s.clientFor(id)
	if err != nil {
		s.render(w, "status.html", page{Title: "Status", Identity: id, Data: view})
		return
	}

	// The namespace from the path, when there is one. Not validated against
	// anything here on purpose: it is handed straight to a read made as the
	// person, and the API server is what decides whether they may see it.
	var opts []client.ListOption
	if ns := r.PathValue("ns"); ns != "" {
		opts = append(opts, client.InNamespace(ns))
		view.Scope = ns
	}

	// The language this customer's own people read, from the customer record.
	//
	// Read before anything is rendered, because every sentence below depends on
	// it. Failing to read it is not an error: English is the fallback and the page
	// still answers the question it exists to answer.
	var tenants doblurav1alpha1.OdooTenantList
	if err := c.List(ctx, &tenants, opts...); err == nil {
		for i := range tenants.Items {
			if lang := tenants.Items[i].Spec.Language; lang != "" {
				view.Locale = localeOf(lang)
				break
			}
		}
	}

	var envs doblurav1alpha1.OdooEnvironmentList
	if err := c.List(ctx, &envs, opts...); err != nil {
		// The API server's own words. A customer cannot act on them, but whoever
		// they forward the screenshot to can, and "something went wrong" cannot
		// be forwarded to anybody.
		view.Denied = err.Error()
		// And the code the answer deserves. This page is the one handed to a
		// customer, so it is also the one somebody points a monitor at, and a
		// refusal served as 200 reads to that monitor as a healthy page.
		code := http.StatusInternalServerError
		if apierrors.IsForbidden(err) {
			code = http.StatusForbidden
		}
		s.render(w, "status.html", page{
			Title: "Status", Identity: id, Data: view, Status: code,
		})
		return
	}

	view.Everything = true
	for i := range envs.Items {
		env := &envs.Items[i]
		row := s.statusOf(ctx, c, id, env, view.Locale)
		if row.State != "up" && row.State != "asleep" {
			view.Everything = false
		}
		view.Rows = append(view.Rows, row)
	}
	sort.Slice(view.Rows, func(i, j int) bool {
		// Production first. On the day it matters, it is the row somebody is
		// looking for, and alphabetical order puts it wherever its name lands.
		pi, pj := purposeRank(view.Rows[i].Purpose), purposeRank(view.Rows[j].Purpose)
		if pi != pj {
			return pi < pj
		}
		return view.Rows[i].Name < view.Rows[j].Name
	})
	view.Multiple = len(view.Rows) > 1
	if len(view.Rows) == 0 {
		view.Everything = false
	}

	s.render(w, "status.html", page{Title: "Status", Identity: id, Data: view})
}

// purposeRank orders the rows by how much somebody cares.
func purposeRank(p string) int {
	switch p {
	case string(doblurav1alpha1.PurposeProduction):
		return 0
	case string(doblurav1alpha1.PurposeStaging):
		return 1
	case string(doblurav1alpha1.PurposeQA):
		return 2
	default:
		return 3
	}
}

// statusOf turns one environment into the row.
func (s *Server) statusOf(
	ctx context.Context,
	c client.Client,
	id Identity,
	env *doblurav1alpha1.OdooEnvironment,
	l locale,
) statusRow {
	row := statusRow{
		Name:     env.Name,
		Customer: env.Spec.ForTenant,
		Purpose:  string(env.Spec.Purpose),
	}

	// Readiness from the Deployment, which is what actually decides whether
	// requests are being served — the environment's own phase says whether
	// doblura finished setting it up, which is a different question and the one
	// that misleads: a Ready environment whose pod crashed an hour ago still says
	// Ready until the operator next looks.
	var deps appsv1.DeploymentList
	known := true
	if err := c.List(ctx, &deps, client.InNamespace(env.Namespace),
		client.MatchingLabels{"doblura.dev/environment": env.Name}); err != nil {
		known = false
	}
	replicas, ready := replicasFor(&deps, env.Name)
	h := environmentHealth(env, replicas, ready, known)
	row.State = h.State
	row.Headline = say(l, "state-"+h.State)
	row.Detail = say(l, "detail-"+h.State)

	if env.Status.URL != "" {
		row.Address = env.Status.URL
	}

	row.Running = h.State == "up" || h.State == "degraded"

	// Load, only if the cluster reports it. See Server.load: a page that says
	// "load: normal" on the strength of nothing is a page that stops being
	// believed the first time it says so during an outage.
	if row.Running {
		if pct, state, ok := s.loadPercent(ctx, id, env); ok {
			row.LoadState = state
			switch state {
			case "down":
				row.Load = say(l, "load-tight")
				row.LoadDetail = fmt.Sprintf(say(l, "load-tight-detail"), pct)
			case "degraded":
				row.Load = say(l, "load-hard")
				row.LoadDetail = fmt.Sprintf(say(l, "load-hard-detail"), pct)
			default:
				row.Load = say(l, "load-comfortable")
				row.LoadDetail = fmt.Sprintf(say(l, "load-comfortable-detail"), pct)
			}
		}
	}

	if since, ok := lastChange(env); ok {
		row.Since = duration(l, since)
	}
	return row
}

// The words for each state used to live in two switch statements here.
//
// They are in the catalogue now, keyed "state-up" and "detail-up" and so on, so
// the sentence and its translation sit beside each other and a test can tell when
// one of them is missing. Nothing about the wording changed: the English in the
// catalogue is what these functions returned.

// lastChange is how long the current state has held.
//
// Returns the duration rather than a formatted string, because the customer page
// has to say it in their language and the operator console in English — and a
// function that formats before it returns forces one of them to unpick it.
func lastChange(env *doblurav1alpha1.OdooEnvironment) (time.Duration, bool) {
	var newest time.Time
	for _, c := range env.Status.Conditions {
		if c.LastTransitionTime.Time.After(newest) {
			newest = c.LastTransitionTime.Time
		}
	}
	if newest.IsZero() {
		return 0, false
	}
	return time.Since(newest), true
}
