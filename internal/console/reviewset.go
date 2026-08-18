// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Antoni Romera

package console

import (
	"net/http"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	doblurav1alpha1 "github.com/doblura/doblura/api/v1alpha1"
)

// One review set, and what it is following.
//
// The list reads like the tab the person came from — pull request numbers and
// titles, linking back to the forge — rather than like a list of branch names,
// because "is there an environment for #3533" is asked by somebody looking at
// #3533.

type reviewSetView struct {
	Set     *doblurav1alpha1.ReviewSet
	State   string
	Word    string
	Watch   string
	Rows    []reviewRow
	CanEdit bool
	// Skipped is repeated out of status because it is the number that explains
	// a set which looks fine and is not doing what somebody expects.
	Skipped int32
}

type reviewRow struct {
	doblurav1alpha1.TrackedRef
	Health health
	Exists bool
}

func (s *Server) handleReviewSet(w http.ResponseWriter, r *http.Request, id Identity) {
	ns, name := r.PathValue("ns"), r.PathValue("name")
	c, err := s.clientFor(id)
	if err != nil {
		s.fail(w, id, err)
		return
	}
	var rs doblurav1alpha1.ReviewSet
	if err := c.Get(r.Context(), client.ObjectKey{Namespace: ns, Name: name}, &rs); err != nil {
		s.fail(w, id, err)
		return
	}

	state, word := reviewSetState(&rs)
	view := reviewSetView{
		Set: &rs, State: state, Word: word,
		Watch: watchSummary(&rs), Skipped: rs.Status.Skipped,
	}

	var envs doblurav1alpha1.OdooEnvironmentList
	_ = c.List(r.Context(), &envs, client.InNamespace(ns),
		client.MatchingLabels{"doblura.dev/review-set": rs.Name})
	deps := listDeployments(r.Context(), c)

	byName := map[string]*doblurav1alpha1.OdooEnvironment{}
	for i := range envs.Items {
		byName[envs.Items[i].Name] = &envs.Items[i]
	}

	for _, t := range rs.Status.Tracked {
		row := reviewRow{TrackedRef: t}
		if e, ok := byName[t.Name]; ok {
			row.Exists = true
			replicas, ready := replicasFor(deps, e.Name)
			row.Health = environmentHealth(e, replicas, ready, deps != nil)
		} else {
			// Tracked but not present: the set has decided it should exist and
			// the environment is not there yet, which is a real state and not an
			// error. Saying "being prepared" is closer than saying nothing.
			row.Health = health{State: "building", Detail: "About to be created."}
		}
		view.Rows = append(view.Rows, row)
	}

	perms, err := s.allowed(r.Context(), id, Verb{"patch", "reviewsets", ns, name})
	if err == nil {
		view.CanEdit = perms[Verb{"patch", "reviewsets", ns, name}.key()]
	}

	s.renderFor(w, r, "reviewset.html", page{
		Title: rs.Name, Identity: id, Data: view,
	})
}

// handleReviewSetPause is the switch somebody reaches for when the noise has to
// stop but the configuration must not be lost.
func (s *Server) handleReviewSetPause(w http.ResponseWriter, r *http.Request, id Identity) {
	ns, name := r.PathValue("ns"), r.PathValue("name")
	c, err := s.clientFor(id)
	if err != nil {
		s.fail(w, id, err)
		return
	}
	var rs doblurav1alpha1.ReviewSet
	if err := c.Get(r.Context(), client.ObjectKey{Namespace: ns, Name: name}, &rs); err != nil {
		s.failBack(w, r, id, err)
		return
	}
	rs.Spec.Paused = r.URL.Query().Get("to") == "paused"
	if err := c.Update(r.Context(), &rs); err != nil {
		s.failBack(w, r, id, err)
		return
	}
	http.Redirect(w, r, "/rs/"+ns+"/"+name, http.StatusSeeOther)
}

// handleCreateReviewSet makes one from the customer's page.
func (s *Server) handleCreateReviewSet(w http.ResponseWriter, r *http.Request, id Identity) {
	ns, tenant := r.PathValue("ns"), r.PathValue("name")
	if err := r.ParseForm(); err != nil {
		s.failBack(w, r, id, err)
		return
	}
	c, err := s.clientFor(id)
	if err != nil {
		s.fail(w, id, err)
		return
	}

	rs := &doblurav1alpha1.ReviewSet{
		ObjectMeta: metav1.ObjectMeta{
			Name: strings.TrimSpace(r.FormValue("name")), Namespace: ns,
		},
		Spec: doblurav1alpha1.ReviewSetSpec{
			ForTenant: tenant,
			Repository: doblurav1alpha1.ReviewRepository{
				Provider: doblurav1alpha1.ForgeProvider(r.FormValue("provider")),
				URL:      strings.TrimSpace(r.FormValue("url")),
				APIBase:  strings.TrimSpace(r.FormValue("apiBase")),
			},
			Watch: doblurav1alpha1.ReviewWatch{
				PullRequests: r.FormValue("pullRequests") == "on",
			},
			Template: doblurav1alpha1.ReviewTemplate{
				Purpose:  doblurav1alpha1.EnvPurpose(r.FormValue("purpose")),
				ImageRef: r.FormValue("imageRef"),
			},
		},
	}
	if v, ok := formInt(r, "maxEnvironments"); ok && v > 0 {
		rs.Spec.MaxEnvironments = v
	}
	if secret := strings.TrimSpace(r.FormValue("authSecret")); secret != "" {
		rs.Spec.Repository.Auth = &doblurav1alpha1.GitAuth{
			Type: doblurav1alpha1.AuthToken, SecretRef: secret,
		}
	}
	for _, b := range strings.Split(r.FormValue("branches"), ",") {
		if b = strings.TrimSpace(b); b != "" {
			rs.Spec.Watch.Branches = append(rs.Spec.Watch.Branches, b)
		}
	}
	for _, l := range strings.Split(r.FormValue("labels"), ",") {
		if l = strings.TrimSpace(l); l != "" {
			rs.Spec.Watch.Labels = append(rs.Spec.Watch.Labels, l)
		}
	}

	if err := c.Create(r.Context(), rs); err != nil {
		s.failBack(w, r, id, err)
		return
	}
	http.Redirect(w, r, "/rs/"+ns+"/"+rs.Name, http.StatusSeeOther)
}
