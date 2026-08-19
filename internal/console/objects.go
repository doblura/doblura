// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Antoni Romera

package console

import (
	"context"
	"net/http"
	"sort"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
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
		Columns: []string{"Name", "State", "Source", "Taken", "Message"},
	},
	{
		Slug: "databases", Title: "Databases", Resource: "odoodatabases",
		Lede: "The databases the platform knows about, and which customers share them.",
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
}

func text(s string) cell  { return cell{Text: s} }
func muted(s string) cell { return cell{Text: s, Muted: true} }
func pill(s string) cell { return cell{State: s} }

// verdict is a pill whose word belongs to its own kind.
func verdict(state, word string) cell { return cell{State: state, Word: word} }

type objectsView struct {
	Kind *objectKind
	Rows []objectRow
	// Denied is set when the person may not list this kind at all, which is a
	// different page from an empty one.
	Denied bool
}

func (s *Server) handleObjects(w http.ResponseWriter, r *http.Request, id Identity) {
	kind := kindBySlug(r.PathValue("kind"))
	if kind == nil {
		http.NotFound(w, r)
		return
	}
	c, err := s.clientFor(id)
	if err != nil {
		s.fail(w, id, err)
		return
	}

	view := objectsView{Kind: kind}
	ctx := r.Context()

	switch kind.Slug {
	case "environments":
		var l doblurav1alpha1.OdooEnvironmentList
		if err := c.List(ctx, &l); err != nil {
			view.Denied = true
			break
		}
		deps := listDeployments(ctx, c)
		for i := range l.Items {
			e := &l.Items[i]
			replicas, ready := replicasFor(deps, e.Name)
			h := environmentHealth(e, replicas, ready, deps != nil)
			addr := "internal only"
			if e.Status.URL != "" {
				addr = e.Status.URL
			}
			view.Rows = append(view.Rows, objectRow{
				Name: e.Name, Namespace: e.Namespace,
				Href: "/e/" + e.Namespace + "/" + e.Name,
				Cells: []cell{
					text(e.Spec.ForTenant),
					pill(h.State),
					text(string(e.Spec.Data.Type) + " data, " + lowerFirst(string(e.Spec.Lifecycle.Type))),
					muted(addr),
				},
			})
		}
	case "reviewsets":
		var l doblurav1alpha1.ReviewSetList
		if err := c.List(ctx, &l); err != nil {
			view.Denied = true
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
		if err := c.List(ctx, &l); err != nil {
			view.Denied = true
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
		if err := c.List(ctx, &l); err != nil {
			view.Denied = true
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
		if err := c.List(ctx, &l); err != nil {
			view.Denied = true
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
					muted(sn.Status.Message),
				},
			})
		}
	case "databases":
		var l doblurav1alpha1.OdooDatabaseList
		if err := c.List(ctx, &l); err != nil {
			view.Denied = true
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
		if err := c.List(ctx, &l); err != nil {
			view.Denied = true
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
	s.renderFor(w, r, "objects.html", page{Title: kind.Title, Identity: id, Data: view})
}

// listDeployments returns nil when the person cannot see them, which the health
// function reads as "cannot tell" rather than as "down".
func listDeployments(ctx context.Context, c client.Client) *appsv1.DeploymentList {
	var deps appsv1.DeploymentList
	if err := c.List(ctx, &deps); err != nil {
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
