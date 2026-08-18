// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Antoni Romera

package console

import (
	"bytes"
	"context"
	"net/url"
	"fmt"
	"html/template"
	"net/http"
	"sort"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	metricsv1beta1 "k8s.io/metrics/pkg/apis/metrics/v1beta1"
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
	// Back is where the error page returns to: the page the person was on, so a
	// refusal does not also cost them the edit they were making.
	Back string

	// Level is how much each page says. One application, two depths — never
	// two applications, which drift.
	Level    detailLevel
	Advanced bool
	// LevelBecause is what the level was derived from, so a screen that shows two
	// colleagues different things can say why.
	LevelBecause string
	// Path is the current page, for marking the navigation.
	Path string
	// SSO says whether the sign-in page should offer the identity provider as
	// well as the local form.
	SSO  bool
	Data any
}

// renderFor is render with the request in hand, so the level and the return path
// are filled in once rather than at every call site.
func (s *Server) renderFor(w http.ResponseWriter, r *http.Request, name string, p page) {
	p.Level, p.LevelBecause = s.levelFor(r.Context(), p.Identity)
	p.Advanced = p.Level == levelAdvanced
	p.Path = r.URL.Path
	s.render(w, name, p)
}

func (s *Server) render(w http.ResponseWriter, name string, p page) {
	p.DevMode = s.opt.DevIdentity != ""
	p.AuthMode = s.authMode()
	p.SSO = s.oidc != nil
	// Rendered into a buffer and only then written out. html/template streams as
	// it goes, so a failure halfway through leaves the reader with half a page
	// and the Go error typeset into the middle of a table — which is what
	// happened the first time this shipped: a type mismatch in one column
	// produced a customer page that looked like an environment had a Go error
	// for a description, and silently dropped every row after it.
	var buf bytes.Buffer
	if err := s.tpl.ExecuteTemplate(&buf, name, p); err != nil {
		http.Error(w, "the page could not be rendered: "+err.Error(),
			http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = buf.WriteTo(w)
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

// failBack is fail with somewhere to return to.
//
// The referer, and only when it is a path on this console. An absolute URL from
// a header would make the error page an open redirector, and an error page is
// exactly where somebody clicks without reading.
func (s *Server) failBack(w http.ResponseWriter, r *http.Request, id Identity, err error) {
	back := "/"
	if ref := r.Header.Get("Referer"); ref != "" {
		if u, e := url.Parse(ref); e == nil && u.Host == r.Host && u.Path != "" {
			back = u.Path
		}
	}
	s.render(w, "error.html", page{
		Title: "Not available", Identity: id, Error: err.Error(), Back: back,
	})
}

// ── the customer list, which is the landing view for everyone ──

type customerRow struct {
	Tenant       *doblurav1alpha1.OdooTenant
	Environments int
	Ready        int
	// Version is what the customer runs, preferring the catalogue's default
	// entry over the recorded field: the catalogue is what somebody edits when
	// they change versions, and the other drifts.
	Version string
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
		if e := t.Spec.DefaultImage(); e != nil {
			row.Version = e.OdooVersion
		} else {
			row.Version = t.Spec.OdooVersion
		}
		rows = append(rows, row)
	}
	sort.Slice(rows, func(i, j int) bool {
		return rows[i].Tenant.Name < rows[j].Tenant.Name
	})

	s.renderFor(w, r, "customers.html", page{Title: "Customers", Identity: id, Data: rows})
}

// ── one customer ──

type customerView struct {
	Tenant       *doblurav1alpha1.OdooTenant
	Environments []envRow
	Rehearsals   []doblurav1alpha1.OdooRehearsal
	Quota        int32
	Open         int32
	Map          template.HTML

	// Repos is what this customer's environments load. Resolved here rather than
	// walked in the template: the object nests optionally twice, and a template
	// that has to reach through two maybe-nil levels renders nothing at all when
	// it guesses wrong — which is what it did, showing an empty section instead
	// of an empty state.
	Repos []doblurav1alpha1.AddonRepo
	// CanEditRepos is asked of the API server like every other permission.
	CanEditRepos bool
}

// envRow is an environment plus the two things a list has to say about it that
// the object itself does not: whether it is actually answering, and in words.
type envRow struct {
	Env    *doblurav1alpha1.OdooEnvironment
	Health health
	Expiry string
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
	// Deployments answer the question a phase cannot: Ready means the operator
	// finished building it, which stays true of an environment whose pod died a
	// minute ago. A person asking "is it up" means the pods.
	var deps appsv1.DeploymentList
	workloadVisible := c.List(r.Context(), &deps, client.InNamespace(ns)) == nil

	for i := range envs.Items {
		e := &envs.Items[i]
		if e.Spec.ForTenant != name {
			continue
		}
		replicas, ready := replicasFor(&deps, e.Name)
		view.Environments = append(view.Environments, envRow{
			Env:    e,
			Health: environmentHealth(e, replicas, ready, workloadVisible),
			Expiry: hibernationHint(e),
		})
	}
	view.Rehearsals = rehearsals.Items
	view.Map = customerGraph(&t, view.Environments, rehearsals.Items).render()
	if d := t.Spec.EnvironmentDefaults; d != nil && d.Addons != nil {
		view.Repos = d.Addons.Repos
	}

	perms, err := s.allowed(r.Context(), id,
		CanCreateEnvironment(ns), CanCreateRehearsal(ns), CanReadLogs(ns),
		Verb{"patch", "odootenants", ns, name})
	if err != nil {
		s.fail(w, id, err)
		return
	}
	view.CanEditRepos = perms[Verb{"patch", "odootenants", ns, name}.key()]
	s.renderFor(w, r, "customer.html", page{
		Title: t.Spec.DisplayName, Identity: id, Perms: perms, Data: view,
	})
}

// ── one environment ──

type environmentView struct {
	Env  *doblurav1alpha1.OdooEnvironment
	Keys []conditionRow

	Health health
	Expiry string
	Map    template.HTML

	// Load answers "is it slow", in the only honest way available without a
	// metrics stack: memory against the limit the size class set. Empty when
	// metrics-server is not installed, because a load figure invented from
	// nothing is worse than no load figure.
	Load       string
	LoadDetail string
	LoadState  string

	WebReplicas string
	CronSummary string

	// Form holds the current values, so the settings form opens showing what is
	// there rather than showing blanks that would wipe it on save.
	Form settingsForm

	// Revisions is name → commit, so the template can look one up beside the ref
	// that was asked for without a nested loop.
	Revisions map[string]string
}

// settingsForm is the editable surface, already reduced to what a form field
// needs. The zero value is never meaningful here: every field is read from the
// object first.
type settingsForm struct {
	WebReplicas int32
	WebWorkers  int32
	CronTier    bool
	CronThreads int32
	Public      bool
	Host        string
	AuthType    string
	AuthSecret  string
	NoIndex     bool
	RateLimit   string
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
	var deps appsv1.DeploymentList
	workloadVisible := c.List(r.Context(), &deps, client.InNamespace(ns)) == nil
	replicas, readyN := replicasFor(&deps, env.Name)

	view := environmentView{
		Env:    &env,
		Health: environmentHealth(&env, replicas, readyN, workloadVisible),
		Expiry: hibernationHint(&env),
	}
	if workloadVisible {
		view.WebReplicas = fmt.Sprintf("%d running of %d wanted", readyN, replicas)
		view.CronSummary = cronSummary(&env, &deps)
	} else {
		view.WebReplicas = "not visible with your access"
		view.CronSummary = "not visible with your access"
	}
	view.Load, view.LoadDetail, view.LoadState = s.load(r.Context(), id, &env)
	view.Map = environmentGraph(&env).render()
	view.Form = formFrom(&env)
	view.Revisions = make(map[string]string, len(env.Status.AddonRevisions))
	for _, rev := range env.Status.AddonRevisions {
		view.Revisions[rev.Name] = rev.Revision
	}

	for _, cond := range env.Status.Conditions {
		view.Keys = append(view.Keys, conditionRow{
			Type: cond.Type, Status: string(cond.Status), Reason: cond.Reason,
			Message: cond.Message, Age: humanSince(&cond.LastTransitionTime),
		})
	}
	perms, err := s.allowed(r.Context(), id,
		CanDeleteEnvironment(ns, name), CanApprove(ns, name), CanReadLogs(ns),
		Verb{"patch", "odooenvironments", ns, name})
	if err != nil {
		s.fail(w, id, err)
		return
	}
	s.renderFor(w, r, "environment.html", page{
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
	s.renderFor(w, r, "me.html", page{Title: "Your access", Identity: id, Data: scopes})
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

// replicasFor finds the workload behind an environment.
//
// Matched on the Deployment name, which the operator sets to the environment's
// name — and NOT on labels, because the cron tier deliberately carries a
// different app.kubernetes.io/name so the Service cannot reach it, and a label
// match would either miss it or count it as a web replica.
func replicasFor(deps *appsv1.DeploymentList, name string) (replicas, ready int32) {
	for i := range deps.Items {
		d := &deps.Items[i]
		if d.Name != name {
			continue
		}
		if d.Spec.Replicas != nil {
			replicas = *d.Spec.Replicas
		}
		ready = d.Status.ReadyReplicas
		return replicas, ready
	}
	return 0, 0
}

// customerGraph builds the map of what is joined to what.
func customerGraph(
	t *doblurav1alpha1.OdooTenant,
	envs []envRow,
	rehearsals []doblurav1alpha1.OdooRehearsal,
) *graph {
	g := &graph{}
	root := "t/" + t.Name
	g.Nodes = append(g.Nodes, node{
		ID: root, Kind: "OdooTenant", Label: t.Spec.DisplayName,
		Sub: t.Namespace, State: "ok", Href: "/c/" + t.Namespace + "/" + t.Name,
	})

	for _, row := range envs {
		e := row.Env
		id := "e/" + e.Name
		g.Nodes = append(g.Nodes, node{
			ID: id, Kind: "OdooEnvironment", Label: e.Name,
			Sub:   stateWord(row.Health.State) + " · " + string(e.Spec.Data.Type),
			State: mapState(row.Health.State),
			Href:  "/e/" + e.Namespace + "/" + e.Name,
		})
		// No label on this edge. It used to carry the lifecycle, which the table
		// above already states, and two of them stacked in a 34px gutter were
		// less legible than nothing. Labels are kept only where they say
		// something the rest of the page does not.
		g.Edges = append(g.Edges, edge{From: root, To: id})

		// The database an environment runs on is a real relationship and the one
		// people are most surprised by: two environments on one server is normal,
		// and it is why "just restart the database" is never a safe suggestion.
		if e.Spec.Database.Host != "" {
			db := "db/" + e.Spec.Database.Host
			if !hasNode(g, db) {
				sub := "shared by this customer"
				if e.Spec.Database.ProxyEnabled() {
					sub = "reached through a pooler"
				}
				g.Nodes = append(g.Nodes, node{
					ID: db, Kind: "OdooDatabase", Label: e.Spec.Database.Host,
					Sub: sub, State: "idle",
				})
			}
			g.Edges = append(g.Edges, edge{From: id, To: db})
		}
		if e.Spec.Data.Snapshot != nil {
			sn := "sn/" + e.Name
			g.Nodes = append(g.Nodes, node{
				ID: sn, Kind: "OdooSnapshot", Label: "anonymised copy",
				Sub: string(e.Spec.Data.Type), State: "idle",
			})
			g.Edges = append(g.Edges, edge{From: id, To: sn, Label: "filled from"})
		}
	}

	for i := range rehearsals {
		rh := &rehearsals[i]
		st := "working"
		switch rh.Status.Phase {
		case doblurav1alpha1.PhaseSucceeded:
			st = "ok"
		case doblurav1alpha1.PhaseFailed:
			st = "bad"
		}
		g.Nodes = append(g.Nodes, node{
			ID: "r/" + rh.Name, Kind: "OdooRehearsal", Label: rh.Name,
			Sub: string(rh.Status.Phase), State: st,
		})
		g.Edges = append(g.Edges, edge{From: root, To: "r/" + rh.Name, Label: "evidence"})
	}
	return g
}

func hasNode(g *graph, id string) bool {
	for i := range g.Nodes {
		if g.Nodes[i].ID == id {
			return true
		}
	}
	return false
}

// mapState collapses the health vocabulary onto the four the map draws. The map
// is a picture of structure; the words beside it carry the nuance.
func mapState(h string) string {
	switch h {
	case "up":
		return "ok"
	case "down", "degraded":
		return "bad"
	case "building":
		return "working"
	default:
		return "idle"
	}
}

// cronSummary says, in words, whether scheduled jobs run somewhere separate.
//
// It matters to a non-technical reader more than it looks: "the nightly invoices
// did not go out" and "the site is slow" have the same cause when crons and web
// share a process, and different causes when they do not.
func cronSummary(env *doblurav1alpha1.OdooEnvironment, deps *appsv1.DeploymentList) string {
	if !env.Spec.Workload.SplitsCrons() {
		return "run alongside the web pages"
	}
	_, ready := replicasFor(deps, env.Name+"-cron")
	if ready == 0 {
		return "have their own worker, which is NOT running"
	}
	return "run on their own worker, separate from the web pages"
}

// load reads what the cluster reports, and says where it came from.
//
// metrics.k8s.io, not Prometheus: metrics-server is what a cluster is likely to
// already have, and this is a "should I worry" figure rather than a graph. When
// it is absent the section disappears entirely — the alternative is a page that
// says "load: normal" on the strength of nothing, which is how a status screen
// stops being believed.
func (s *Server) load(
	ctx context.Context,
	id Identity,
	env *doblurav1alpha1.OdooEnvironment,
) (headline, detail, state string) {
	c, err := s.clientFor(id)
	if err != nil {
		return "", "", ""
	}
	var pods corev1.PodList
	if err := c.List(ctx, &pods, client.InNamespace(env.Namespace),
		client.MatchingLabels{"doblura.dev/environment": env.Name}); err != nil {
		return "", "", ""
	}

	limit := sizeMemoryLimit(env.Spec.Size)
	if limit == 0 {
		return "", "", ""
	}

	var metrics metricsv1beta1.PodMetricsList
	if err := c.List(ctx, &metrics, client.InNamespace(env.Namespace)); err != nil {
		// No metrics-server. Say nothing rather than something reassuring.
		return "", "", ""
	}

	var used int64
	var counted int
	for i := range metrics.Items {
		m := &metrics.Items[i]
		if !podInList(&pods, m.Name) {
			continue
		}
		for _, cm := range m.Containers {
			if q := cm.Usage.Memory(); q != nil {
				used += q.Value()
			}
		}
		counted++
	}
	if counted == 0 {
		return "", "", ""
	}

	pct := int(used * 100 / (limit * int64(counted)))
	switch {
	case pct >= 90:
		return "Running out of memory",
			fmt.Sprintf("Using %d%% of what it is allowed. At this level it will be slow, and "+
				"it may be restarted by the cluster. A bigger size or fewer people at once.", pct),
			"down"
	case pct >= 75:
		return "Working hard",
			fmt.Sprintf("Using %d%% of what it is allowed. Complaints about slowness are "+
				"plausible; below 75%% they usually are not.", pct),
			"degraded"
	default:
		return "Comfortable",
			fmt.Sprintf("Using %d%% of the memory it is allowed. If somebody says it is slow, "+
				"the cause is probably not this environment being short of resources.", pct),
			"up"
	}
}

func podInList(pods *corev1.PodList, name string) bool {
	for i := range pods.Items {
		if pods.Items[i].Name == name {
			return true
		}
	}
	return false
}

// sizeMemoryLimit mirrors the operator's own size table. Duplicated rather than
// imported because internal/controller is the operator's business and the
// console must not reach into it; the number is a display aid, and being a
// little out of date is not a correctness problem.
func sizeMemoryLimit(size doblurav1alpha1.Size) int64 {
	const gi = 1024 * 1024 * 1024
	switch size {
	case doblurav1alpha1.SizeSmall:
		return 2 * gi
	case doblurav1alpha1.SizeLarge:
		return 12 * gi
	default:
		return 4 * gi
	}
}

// environmentGraph is the map from one environment's point of view.
func environmentGraph(e *doblurav1alpha1.OdooEnvironment) *graph {
	g := &graph{}
	t := "t/" + e.Spec.ForTenant
	g.Nodes = append(g.Nodes, node{
		ID: t, Kind: "OdooTenant", Label: e.Spec.ForTenant, State: "ok",
		Href: "/c/" + e.Namespace + "/" + e.Spec.ForTenant,
	})
	id := "e/" + e.Name
	g.Nodes = append(g.Nodes, node{
		ID: id, Kind: "OdooEnvironment", Label: e.Name,
		Sub: string(e.Status.Phase), State: mapState(string(e.Status.Phase)),
	})
	g.Edges = append(g.Edges, edge{From: t, To: id})
	if e.Spec.Database.Host != "" {
		sub := "same cluster"
		if e.Spec.Database.ProxyEnabled() {
			sub = "reached through a pooler"
		}
		g.Nodes = append(g.Nodes, node{
			ID: "db", Kind: "OdooDatabase", Label: e.Spec.Database.Host,
			Sub: sub, State: "idle",
		})
		g.Edges = append(g.Edges, edge{From: id, To: "db"})
	}
	if e.Spec.Workload.SplitsCrons() {
		g.Nodes = append(g.Nodes, node{
			ID: "cron", Kind: "OdooEnvironment", Label: e.Name + "-cron",
			Sub: "scheduled jobs", State: "idle",
		})
		g.Edges = append(g.Edges, edge{From: id, To: "cron", Label: "crons"})
	}
	return g
}

// formFrom reads the current settings out of the object.
//
// Every field, including the ones with schema defaults, because the form posts
// all of them: a field left blank because "it is the default anyway" would be
// read back as "not set" and clear whatever the operator had filled in.
func formFrom(e *doblurav1alpha1.OdooEnvironment) settingsForm {
	f := settingsForm{
		WebReplicas: 1,
		WebWorkers:  2,
		CronThreads: 2,
		Host:        e.Spec.Exposure.Host,
		AuthType:    string(e.Spec.Exposure.Auth.Type),
		AuthSecret:  e.Spec.Exposure.Auth.SecretRef,
		Public:      e.Spec.IsPublic(),
		NoIndex:     e.Spec.Exposure.NoIndex == nil || *e.Spec.Exposure.NoIndex,
	}
	if w := e.Spec.Workload; w != nil {
		if w.Web != nil {
			f.WebReplicas = w.Web.Replicas
			if w.Web.Workers != nil {
				f.WebWorkers = *w.Web.Workers
			}
		}
		if w.Cron != nil && w.Cron.Replicas > 0 {
			f.CronTier = true
			if w.Cron.Threads > 0 {
				f.CronThreads = w.Cron.Threads
			}
		}
	}
	if e.Spec.Exposure.RateLimitRPS != nil && *e.Spec.Exposure.RateLimitRPS > 0 {
		f.RateLimit = itoa(int(*e.Spec.Exposure.RateLimitRPS))
	}
	if f.AuthType == "" {
		f.AuthType = "None"
	}
	return f
}
