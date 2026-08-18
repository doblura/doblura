// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Antoni Romera

package console

import (
	"net/http"
	"strings"

	"sigs.k8s.io/controller-runtime/pkg/client"

	doblurav1alpha1 "github.com/doblura/doblura/api/v1alpha1"
)

// Adding and removing the modules a customer loads.
//
// On the CUSTOMER, not on each environment. A customer runs the same handful of
// OCA repositories and the same private one across every review environment they
// open, and restating that per environment is how one of them quietly ends up
// with a different set — noticed when a module is missing in staging and present
// in production.
//
// The form asks for four things and no more: a name, a URL, a ref, and how to
// authenticate. Everything else the API supports has a working default, and a
// form that shows every field is a form nobody finishes.

func (s *Server) handleAddRepo(w http.ResponseWriter, r *http.Request, id Identity) {
	ns, name := r.PathValue("ns"), r.PathValue("name")
	if err := r.ParseForm(); err != nil {
		s.failBack(w, r, id, err)
		return
	}
	c, err := s.clientFor(id)
	if err != nil {
		s.fail(w, id, err)
		return
	}

	var t doblurav1alpha1.OdooTenant
	if err := c.Get(r.Context(), client.ObjectKey{Namespace: ns, Name: name}, &t); err != nil {
		s.failBack(w, r, id, err)
		return
	}
	if t.Spec.EnvironmentDefaults == nil {
		t.Spec.EnvironmentDefaults = &doblurav1alpha1.EnvironmentDefaults{}
	}
	if t.Spec.EnvironmentDefaults.Addons == nil {
		t.Spec.EnvironmentDefaults.Addons = &doblurav1alpha1.AddonsSpec{}
	}
	a := t.Spec.EnvironmentDefaults.Addons

	repo := doblurav1alpha1.AddonRepo{
		Name:  strings.TrimSpace(r.FormValue("repoName")),
		URL:   strings.TrimSpace(r.FormValue("url")),
		Ref:   strings.TrimSpace(r.FormValue("ref")),
		Depth: 1,
	}
	if p := strings.TrimSpace(r.FormValue("paths")); p != "" {
		for _, part := range strings.Split(p, ",") {
			if part = strings.TrimSpace(part); part != "" {
				repo.Paths = append(repo.Paths, part)
			}
		}
	}
	if at := r.FormValue("authType"); at != "" && at != "Public" {
		repo.Auth = &doblurav1alpha1.GitAuth{
			Type:      doblurav1alpha1.GitAuthType(at),
			SecretRef: strings.TrimSpace(r.FormValue("authSecret")),
			Username:  strings.TrimSpace(r.FormValue("authUser")),
		}
	}

	// Replace by name rather than append: adding a repository that is already
	// there is how somebody edits one, and appending a duplicate would produce a
	// second entry cloning over the first.
	replaced := false
	for i := range a.Repos {
		if a.Repos[i].Name == repo.Name {
			a.Repos[i] = repo
			replaced = true
			break
		}
	}
	if !replaced {
		a.Repos = append(a.Repos, repo)
	}

	if err := c.Update(r.Context(), &t); err != nil {
		s.failBack(w, r, id, err)
		return
	}
	http.Redirect(w, r, "/c/"+ns+"/"+name, http.StatusSeeOther)
}

func (s *Server) handleRemoveRepo(w http.ResponseWriter, r *http.Request, id Identity) {
	ns, name := r.PathValue("ns"), r.PathValue("name")
	if err := r.ParseForm(); err != nil {
		s.failBack(w, r, id, err)
		return
	}
	c, err := s.clientFor(id)
	if err != nil {
		s.fail(w, id, err)
		return
	}
	var t doblurav1alpha1.OdooTenant
	if err := c.Get(r.Context(), client.ObjectKey{Namespace: ns, Name: name}, &t); err != nil {
		s.failBack(w, r, id, err)
		return
	}
	d := t.Spec.EnvironmentDefaults
	if d == nil || d.Addons == nil {
		http.Redirect(w, r, "/c/"+ns+"/"+name, http.StatusSeeOther)
		return
	}

	// Removing here changes what NEW environments get. The ones already running
	// keep the repositories they were built with, because their spec was filled
	// in when they were created — which is the behaviour somebody wants, and is
	// worth saying on the page rather than leaving them to discover.
	want := r.FormValue("repoName")
	kept := d.Addons.Repos[:0]
	for _, repo := range d.Addons.Repos {
		if repo.Name != want {
			kept = append(kept, repo)
		}
	}
	d.Addons.Repos = kept

	if err := c.Update(r.Context(), &t); err != nil {
		s.failBack(w, r, id, err)
		return
	}
	http.Redirect(w, r, "/c/"+ns+"/"+name, http.StatusSeeOther)
}
