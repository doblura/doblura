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

	appsv1 "k8s.io/api/apps/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	doblurav1alpha1 "github.com/doblura/doblura/api/v1alpha1"
)

// The other kinds.
//
// The console started with customers and environments because those are what
// support touches. But the operator manages seven kinds, and a console that shows
// two of them is a console that sends people back to kubectl for the other five —
// which is the same as not having one, for the people who cannot use kubectl.
//
// One handler and one template rather than five of each. The kinds differ in
// their columns and in nothing else that matters here: they are all namespaced
// lists with a name, a state and a couple of facts. Five near-identical pages
// would drift, and the drift would be in the parts nobody looks at.

// objectKind describes a listing.
type objectKind struct {
	Slug     string // in the URL
	Title    string
	Resource string // for the access review
	Lede     string
	Columns  []string
}

var objectKinds = []objectKind{
	{
		Slug: "environments", Title: "Environments", Resource: "odooenvironments",
		Lede: "Every environment across every customer you can see. A customer's " +
			"own page is usually the better place to start.",
		Columns: []string{"Name", "Customer", "State", "What it is", "Address"},
	},
	{
		Slug: "reviewsets", Title: "Review sets", Resource: "reviewsets",
		Lede: "Each one watches a repository and opens an environment per pull " +
			"request, so the decision made when the pull request was opened is " +
			"not one somebody has to mirror by hand.",
		Columns: []string{"Name", "Customer", "State", "Watching", "Open"},
	},
	{
		Slug: "rehearsals", Title: "Rehearsals", Resource: "odoorehearsals",
		Lede: "A rehearsal restores a copy and runs the migration against it. It is " +
			"what makes a version change something you have already seen happen.",
		Columns: []string{"Name", "State", "Image", "Started", "Message"},
	},
	{
		Slug: "backups", Title: "Backups", Resource: "odoobackups",
		Lede: "Copies kept to put back. Not snapshots — a snapshot is anonymised " +
			"so a rehearsal can hold realistic data; a backup is the original.",
		Columns: []string{"Name", "Environment", "State", "Kept", "Last success"},
	},
	{
		Slug: "snapshots", Title: "Snapshots", Resource: "odoosnapshots",
		Lede: "Anonymised copies of production. Every one of these is customer data " +
			"with the names changed, which is not the same as customer data removed.",
		// "What was masked" rather than "Message". The message repeated the state
		// pill next to it, and the question this page exists to answer — how much
		// of the personal data in this copy was actually changed — was not on it
		// at all. On a failed snapshot the same column carries the reason, because
		// that is what somebody is looking for on that row.
		Columns: []string{"Name", "State", "Source", "Taken", "What was masked"},
	},
	{
		Slug: "builds", Title: "Builds", Resource: "odoobuilds",
		Lede: "Images built from the customer's own repositories, in the customer's " +
			"own cluster. What each one is worth knowing by is its digest and the " +
			"commit it came from, not the tag somebody can move.",
		Columns: []string{"Name", "State", "Customer", "Built", "What went in"},
	},
	{
		Slug: "databases", Title: "Databases", Resource: "odoodatabases",
		Lede:    "The databases the platform knows about, and which customers share them.",
		Columns: []string{"Name", "Tenancy", "Companies", "Size"},
	},
	{
		Slug: "servers", Title: "Servers", Resource: "odooinstances",
		Lede: "The Postgres servers environments are placed on, and how much room " +
			"each has left.",
		Columns: []string{"Name", "Class", "Region", "Disk free", "Last seen"},
	},
}

func kindBySlug(slug string) *objectKind {
	for i := range objectKinds {
		if objectKinds[i].Slug == slug {
			return &objectKinds[i]
		}
	}
	return nil
}

// objectRow is one line, already reduced to strings.
//
// Rendered to strings here rather than in the template, because the alternative
// is a template full of type switches — and a template that cannot fail on a
// type is a template that renders instead of erroring.
type objectRow struct {
	Name      string
	Namespace string
	Href      string
	// Cluster is where it is, shown only in the aggregated view.
	Cluster string
	// Cells are the columns after the name, in order. A cell is either text or a
	// state pill — resolved here so the template is a loop and nothing else. A
	// template that has to decide what a value IS ends up full of type switches,
	// and a template cannot fail on a type: it renders something wrong instead.
	Cells []cell
}

type cell struct {
	Text  string
	State string // when set, the cell is a state pill
	// Word overrides the state's default label. A rehearsal is not "Up": it
	// passed or it failed. Sharing the colours across kinds is right — green
	// means the same thing everywhere — but sharing the nouns is not.
	Word  string
	Muted bool
	// Link makes the cell's text a link to somewhere else.
	//
	// An address in a list is the thing somebody wants to click, and it was plain
	// grey text — while the same address on the environment's own page was a link.
	// Two pages disagreeing about whether a URL is clickable is the kind of small
	// inconsistency that makes an interface feel unfinished.
	Link string
}

func text(s string) cell  { return cell{Text: s} }
func muted(s string) cell { return cell{Text: s, Muted: true} }

// buildState maps a build phase onto the shared colour vocabulary.
func buildState(p string) string {
	switch p {
	case string(doblurav1alpha1.BuildSucceeded):
		return "up"
	case string(doblurav1alpha1.BuildFailed):
		return "down"
	case "":
		return "unknown"
	default:
		return "building"
	}
}

// sourcesCell says what went into the image.
//
// The commit and not the ref: a build from a branch is not reproducible, and the
// commit is the only thing that answers "which code is in this image" a year
// later. A build that succeeded and recorded no commit says so, because that is a
// build nobody can trace back.
func sourcesCell(b *doblurav1alpha1.OdooBuild) cell {
	if len(b.Status.Sources) == 0 {
		if b.Status.Phase == doblurav1alpha1.BuildSucceeded {
			return cell{State: "degraded", Word: "nothing recorded"}
		}
		return muted("")
	}
	var parts []string
	for _, s := range b.Status.Sources {
		switch {
		case s.Commit == "":
			parts = append(parts, s.Name+" @ "+s.Ref+" (no commit recorded)")
		default:
			parts = append(parts, s.Name+" @ "+s.Commit[:min(8, len(s.Commit))])
		}
	}
	return muted(strings.Join(parts, ", "))
}

// maskingCell answers the question the snapshots page is for: how much of the
// personal data in this copy was actually changed.
//
// The number the object used to carry was the count of RULES — computed from the
// spec, before anything ran, and reported as evidence. On a database missing the
// tables some rules name it read far higher than what happened. So this shows
// both numbers when they differ, and names the gap rather than averaging it away:
// a column that was not masked is a column whose real values are in that dump.
func maskingCell(sn *doblurav1alpha1.OdooSnapshot) cell {
	st := sn.Status
	if st.Phase != doblurav1alpha1.SnapSucceeded {
		// The reason, which is what somebody is looking for on a row that failed.
		return muted(st.Message)
	}
	if st.ColumnsMasked == 0 && st.ColumnsDeclared == 0 {
		return muted("nothing declared to mask")
	}
	if st.ColumnsMasked == 0 {
		// A copy of production with nothing changed in it. Never quietly.
		return cell{State: "down", Word: "nothing was masked"}
	}
	if len(st.NotMasked) == 0 {
		return muted(fmt.Sprintf("%d columns", st.ColumnsMasked))
	}
	return cell{
		State: "degraded",
		Word: fmt.Sprintf("%d of %d — %s not in this database",
			st.ColumnsMasked, st.ColumnsDeclared, plural(len(st.NotMasked), "rule")),
	}
}

// address is a URL somebody can open, or grey text when there is none.
//
// External, so it opens in its own tab and carries noopener: an environment is
// somebody's Odoo, and losing the console to it is a small annoyance every time.
func address(url string) cell {
	if url == "" {
		return muted("internal only")
	}
	return cell{Text: url, Link: url}
}
func pill(s string) cell { return cell{State: s} }

// verdict is a pill whose word belongs to its own kind.
func verdict(state, word string) cell { return cell{State: state, Word: word} }

// Denied means REFUSED, and nothing else.
//
// It was set on any list error at all, so a cluster that could not be reached was
// reported to the person as "your groups do not permit reading these" — which
// sends them to ask for permissions they already have, about a cluster that is
// simply down. Only a Forbidden is a permissions answer; everything else is the
// cluster not answering, and says so in the API server's own words.
type objectsView struct {
	Kind *objectKind
	Rows []objectRow
	// Denied is set when the person may not list this kind at all, which is a
	// different page from an empty one.
	Denied bool
	// refusal is the API server's own words for that Denied, kept so the handler
	// can hand them to the "which customer?" page. Unexported: it is for the
	// handler's decision, not for a template to print — the template already has
	// its own sentence for a refusal.
	refusal error

	// Cluster is which one these rows came from, when they came from one.
	Cluster string
	// Everywhere means the rows are from every cluster and carry their own.
	Everywhere bool
	// Troubles are the clusters that could not be asked. Shown, never dropped: a
	// federated list that quietly returns fewer rows reports an outage as calm.
	Troubles []clusterTrouble
}

// shouldAskForScope is whether this refusal is really a question.
//
// A function rather than a condition inline, because this exact answer has been
// lost once already — a refactor moved the List out of the overview's handler and
// took the ask with it — and a condition in a handler cannot be tested without an
// API server, which is why nothing caught it.
func (v objectsView) shouldAskForScope(r *http.Request) bool {
	return v.Denied && clusterWideRefusal(r, v.refusal)
}

func (s *Server) handleObjects(w http.ResponseWriter, r *http.Request, id Identity) {
	kind := kindBySlug(r.PathValue("kind"))
	if kind == nil {
		http.NotFound(w, r)
		return
	}
	ctx := r.Context()
	// One scope for every kind on this page. These lists are cluster-wide, which a
	// RoleBinding to a single namespace does not permit — see scope.go.
	scope := scopeOption(r)

	// Every cluster at once, when that is what was chosen. Each row then carries
	// where it is, and a cluster that could not be asked is shown as itself
	// rather than as fewer rows — see fanout.go.
	if s.Everywhere(r) {
		results := fanOut(ctx, s, id, func(ctx context.Context, who Identity) (objectsView, error) {
			return s.objectsIn(ctx, who, scope, kind)
		})
		s.renderFor(w, r, "objects.html", page{
			Title: kind.Title, Identity: id, Data: mergeObjects(kind, results),
		})
		return
	}

	view, err := s.objectsIn(ctx, id, scope, kind)
	if err != nil {
		s.fail(w, id, err)
		return
	}
	// The same answer the overview gives, for the same reason: these lists are
	// cluster-wide, a RoleBinding to one namespace never permits a cluster-wide
	// list, and telling that person "your groups do not permit reading these"
	// sends them to ask for access they already have. It was wired into the
	// overview alone, so anybody who reached the rail instead of the front page
	// met the refusal the fix exists to prevent.
	if view.shouldAskForScope(r) {
		s.askForScope(w, r, id, view.refusal)
		return
	}
	s.renderFor(w, r, "objects.html", page{Title: kind.Title, Identity: id, Data: view})
}

// objectsIn is one cluster's worth of rows.
//
// Split out of the handler so the same code answers one cluster and all of them:
// a second implementation for the aggregated view would drift, and the drift
// would show as one list disagreeing with another about the same object.
func (s *Server) objectsIn(
	ctx context.Context,
	id Identity,
	scope client.ListOption,
	kind *objectKind,
) (objectsView, error) {
	c, err := s.clientFor(id)
	if err != nil {
		return objectsView{Kind: kind}, err
	}
	view := objectsView{Kind: kind, Cluster: id.Cluster}

	switch kind.Slug {
	case "environments":
		var l doblurav1alpha1.OdooEnvironmentList
		if err := c.List(ctx, &l, scope); err != nil {
			if !apierrors.IsForbidden(err) {
				return view, err
			}
			view.Denied, view.refusal = true, err
			break
		}
		deps := listDeployments(ctx, c, scope)
		for i := range l.Items {
			e := &l.Items[i]
			replicas, ready := replicasFor(deps, e.Name)
			h := environmentHealth(e, replicas, ready, deps != nil)

			view.Rows = append(view.Rows, objectRow{
				Name: e.Name, Namespace: e.Namespace,
				Href: "/e/" + e.Namespace + "/" + e.Name,
				Cells: []cell{
					text(e.Spec.ForTenant),
					pill(h.State),
					text(string(e.Spec.Data.Type) + " data, " + lowerFirst(string(e.Spec.Lifecycle.Type))),
					address(e.Status.URL),
				},
			})
		}
	case "reviewsets":
		var l doblurav1alpha1.ReviewSetList
		if err := c.List(ctx, &l, scope); err != nil {
			if !apierrors.IsForbidden(err) {
				return view, err
			}
			view.Denied, view.refusal = true, err
			break
		}
		for i := range l.Items {
			rs := &l.Items[i]
			state, word := reviewSetState(rs)
			open := itoa(int(rs.Status.Open))
			if rs.Status.Skipped > 0 {
				// The skipped count travels with the open one, because a set at
				// its cap looks identical to a healthy one from the number alone.
				open += " (" + itoa(int(rs.Status.Skipped)) + " over the cap)"
			}
			view.Rows = append(view.Rows, objectRow{
				Name: rs.Name, Namespace: rs.Namespace,
				Href: "/rs/" + rs.Namespace + "/" + rs.Name,
				Cells: []cell{
					text(rs.Spec.ForTenant),
					verdict(state, word),
					muted(watchSummary(rs)),
					text(open),
				},
			})
		}
	case "rehearsals":
		var l doblurav1alpha1.OdooRehearsalList
		if err := c.List(ctx, &l, scope); err != nil {
			if !apierrors.IsForbidden(err) {
				return view, err
			}
			view.Denied, view.refusal = true, err
			break
		}
		for i := range l.Items {
			rh := &l.Items[i]
			view.Rows = append(view.Rows, objectRow{
				Name: rh.Name, Namespace: rh.Namespace,
				Cells: []cell{
					verdict(rehearsalState(rh.Status.Phase), rehearsalWord(rh.Status.Phase)),
					muted(rh.Spec.Image),
					muted(humanSince(rh.Status.StartedAt)),
					muted(rh.Status.Message),
				},
			})
		}
	case "backups":
		var l doblurav1alpha1.OdooBackupList
		if err := c.List(ctx, &l, scope); err != nil {
			if !apierrors.IsForbidden(err) {
				return view, err
			}
			view.Denied, view.refusal = true, err
			break
		}
		for i := range l.Items {
			b := &l.Items[i]
			state, word := backupState(b)
			kept := itoa(int(b.Status.Kept))
			if n := len(b.Status.Pending); n > 0 {
				kept += " (" + itoa(n) + " going)"
			}
			view.Rows = append(view.Rows, objectRow{
				Name: b.Name, Namespace: b.Namespace,
				Href: "/b/" + b.Namespace + "/" + b.Name,
				Cells: []cell{
					text(b.Spec.Environment),
					verdict(state, word),
					text(kept),
					muted(humanSince(b.Status.LastSuccess)),
				},
			})
		}
	case "snapshots":
		var l doblurav1alpha1.OdooSnapshotList
		if err := c.List(ctx, &l, scope); err != nil {
			if !apierrors.IsForbidden(err) {
				return view, err
			}
			view.Denied, view.refusal = true, err
			break
		}
		for i := range l.Items {
			sn := &l.Items[i]
			view.Rows = append(view.Rows, objectRow{
				Name: sn.Name, Namespace: sn.Namespace,
				Cells: []cell{
					verdict(snapshotState(string(sn.Status.Phase)), snapshotWord(string(sn.Status.Phase))),
					muted(sn.Spec.Source.Host),
					muted(humanSince(sn.Status.LastSuccessfulTime)),
					maskingCell(sn),
				},
			})
		}
	case "builds":
		var l doblurav1alpha1.OdooBuildList
		if err := c.List(ctx, &l, scope); err != nil {
			if !apierrors.IsForbidden(err) {
				return view, err
			}
			view.Denied, view.refusal = true, err
			break
		}
		for i := range l.Items {
			b := &l.Items[i]
			view.Rows = append(view.Rows, objectRow{
				Name: b.Name, Namespace: b.Namespace,
				Cells: []cell{
					verdict(buildState(string(b.Status.Phase)), string(b.Status.Phase)),
					muted(b.Spec.ForTenant),
					muted(humanSince(b.Status.BuiltAt)),
					sourcesCell(b),
				},
			})
		}
	case "databases":
		var l doblurav1alpha1.OdooDatabaseList
		if err := c.List(ctx, &l, scope); err != nil {
			if !apierrors.IsForbidden(err) {
				return view, err
			}
			view.Denied, view.refusal = true, err
			break
		}
		for i := range l.Items {
			db := &l.Items[i]
			view.Rows = append(view.Rows, objectRow{
				Name: db.Name, Namespace: db.Namespace,
				Cells: []cell{
					text(string(db.Spec.Tenancy)),
					text(itoa(len(db.Spec.Companies)) + " on this database"),
					muted(gib(db.Spec.SizeGi)),
				},
			})
		}
	case "servers":
		var l doblurav1alpha1.OdooInstanceList
		if err := c.List(ctx, &l, scope); err != nil {
			if !apierrors.IsForbidden(err) {
				return view, err
			}
			view.Denied, view.refusal = true, err
			break
		}
		for i := range l.Items {
			in := &l.Items[i]
			view.Rows = append(view.Rows, objectRow{
				Name: in.Name, Namespace: in.Namespace,
				Cells: []cell{
					text(string(in.Spec.Class)),
					text(in.Spec.Region),
					muted(gib(in.Status.DiskFreeGi)),
					muted(humanSince(in.Status.LastProbe)),
				},
			})
		}
	}

	sort.Slice(view.Rows, func(i, j int) bool {
		if view.Rows[i].Namespace != view.Rows[j].Namespace {
			return view.Rows[i].Namespace < view.Rows[j].Namespace
		}
		return view.Rows[i].Name < view.Rows[j].Name
	})
	return view, nil
}

// mergeObjects joins what every cluster answered.
//
// Denied is a property of a cluster and not of the page: a person may read
// environments in one and not in another, and collapsing that into one flag would
// either hide the rows they can see or claim they can see none.
func mergeObjects(kind *objectKind, results []clusterResult[objectsView]) objectsView {
	out := objectsView{Kind: kind, Everywhere: true, Troubles: troubles(results)}
	for _, res := range results {
		if res.Err != nil {
			continue
		}
		if res.Value.Denied {
			out.Troubles = append(out.Troubles, clusterTrouble{
				Cluster: res.Cluster,
				Why:     "your groups do not permit reading these in this cluster",
			})
			continue
		}
		for _, row := range res.Value.Rows {
			row.Cluster = res.Cluster
			// The link carries its cluster, because following it lands on a page
			// about ONE object and that page has to ask the right API server.
			// Through /cluster so the choice is remembered, which is what makes
			// the back button behave.
			if row.Href != "" {
				row.Href = "/cluster?to=" + url.QueryEscape(res.Cluster) +
					"&back=" + url.QueryEscape(row.Href)
			}
			out.Rows = append(out.Rows, row)
		}
	}
	return out
}

// listDeployments returns nil when the person cannot see them, which the health
// function reads as "cannot tell" rather than as "down".
func listDeployments(ctx context.Context, c client.Client, scope client.ListOption) *appsv1.DeploymentList {
	var deps appsv1.DeploymentList
	if err := c.List(ctx, &deps, scope); err != nil {
		return nil
	}
	return &deps
}

func rehearsalState(p doblurav1alpha1.RehearsalPhase) string {
	switch p {
	case doblurav1alpha1.PhaseSucceeded:
		return "up"
	case doblurav1alpha1.PhaseFailed:
		return "down"
	case "":
		return "building"
	default:
		return "building"
	}
}

// rehearsalWord is what happened, in the words somebody would use about a
// rehearsal rather than about a server.
func rehearsalWord(p doblurav1alpha1.RehearsalPhase) string {
	switch p {
	case doblurav1alpha1.PhaseSucceeded:
		return "Passed"
	case doblurav1alpha1.PhaseFailed:
		return "Failed"
	case "":
		return "Not started"
	default:
		return string(p)
	}
}

func snapshotWord(p string) string {
	switch p {
	case "Succeeded", "Ready", "Available":
		return "Taken"
	case "Failed":
		return "Failed"
	case "":
		return "Not started"
	default:
		return p
	}
}

func snapshotState(p string) string {
	switch p {
	case "Succeeded", "Ready", "Available":
		return "up"
	case "Failed":
		return "down"
	default:
		return "building"
	}
}

func gib(v *int32) string {
	if v == nil {
		return "—"
	}
	return itoa(int(*v)) + " GiB"
}

func lowerFirst(s string) string {
	if s == "" {
		return s
	}
	b := []byte(s)
	if b[0] >= 'A' && b[0] <= 'Z' {
		b[0] += 'a' - 'A'
	}
	return string(b)
}

// reviewSetState reads the Watching condition into the shared vocabulary.
//
// Paused is deliberately NOT "down": somebody paused it on purpose, and a red
// pill for a decision a colleague made is how a status page trains people to
// ignore it.
func reviewSetState(rs *doblurav1alpha1.ReviewSet) (state, word string) {
	if rs.Spec.Paused {
		return "asleep", "Paused"
	}
	for _, c := range rs.Status.Conditions {
		if c.Type != "Watching" {
			continue
		}
		if c.Status == "True" {
			return "up", "Watching"
		}
		return "down", "Not watching"
	}
	return "building", "Starting"
}

// watchSummary says what it is watching, in words.
func watchSummary(rs *doblurav1alpha1.ReviewSet) string {
	var parts []string
	if rs.Spec.Watch.PullRequests {
		p := "pull requests"
		if len(rs.Spec.Watch.Labels) > 0 {
			p += " labelled " + strings.Join(rs.Spec.Watch.Labels, " or ")
		}
		parts = append(parts, p)
	}
	if len(rs.Spec.Watch.Branches) > 0 {
		parts = append(parts, "branches "+strings.Join(rs.Spec.Watch.Branches, ", "))
	}
	if len(parts) == 0 {
		return "nothing"
	}
	return strings.Join(parts, " and ")
}
