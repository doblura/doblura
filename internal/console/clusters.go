// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Antoni Romera

package console

import (
	"context"
	"fmt"
	"net/http"
	"sort"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// More than one cluster.
//
// The shape here is decision 4 in DECISIONS.md, and the thing it deliberately is
// NOT matters more than what it is: this is not a control plane that writes to
// remote clusters with stored credentials. That is the version everybody draws
// first, and it creates exactly the asset an attacker wants — one place holding
// write access to every customer's cluster.
//
// Instead the console holds, in each cluster, the same credential it holds
// locally: permission to impersonate, and nothing else. What a person may do in a
// cluster is answered by that cluster's own RBAC, as the person. Choosing a
// cluster changes which API server is asked; it never changes what the answer is
// allowed to be.
//
// A single-cluster install must be unchanged by all of this — no picker, no
// banner, no mention. That is decision 3: the open project stays completely usable
// on its own, with no degraded mode and no reminder that a paid thing exists.

const clusterCookie = "doblura_cluster"

// allClusters is the picker's "everywhere" entry.
//
// A sentinel and not the empty string, which already means "the local one". The
// lists that can answer it do; the pages that cannot — anything about one object
// — carry a cluster in their link instead, so following one from an aggregated
// list lands in the right place.
const allClusters = "*"

// loadClusters reads a kubeconfig per key from the Secret.
//
// Read once at startup rather than per request. A console that re-read them would
// pick up a rotated credential without a restart, which sounds good until the
// failure mode: a Secret edited wrongly takes every cluster away mid-session, and
// nothing on screen would say why.
func loadClusters(
	ctx context.Context,
	c client.Client,
	namespace, name string,
) (map[string]*rest.Config, error) {
	out := map[string]*rest.Config{}
	if name == "" {
		return out, nil
	}

	var sec corev1.Secret
	if err := c.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, &sec); err != nil {
		return nil, fmt.Errorf("reading the clusters secret %s/%s: %w", namespace, name, err)
	}

	for key, raw := range sec.Data {
		cfg, err := clientcmd.RESTConfigFromKubeConfig(raw)
		if err != nil {
			// Named and refused, not skipped. A cluster that quietly fails to
			// load is a cluster somebody thinks they are looking at.
			return nil, fmt.Errorf("the kubeconfig for cluster %q cannot be used: %w", key, err)
		}
		out[key] = cfg
	}
	return out, nil
}

// clusterOf is which cluster this request is about.
func (s *Server) clusterOf(r *http.Request) string {
	c, err := r.Cookie(clusterCookie)
	if err != nil {
		return s.opt.LocalClusterName
	}
	// Validated against what exists, so a stale cookie from a cluster that has
	// been removed lands on the local one rather than on an error page.
	if c.Value == s.opt.LocalClusterName || (c.Value == allClusters && s.Federated()) {
		return c.Value
	}
	if _, ok := s.clusters[c.Value]; ok {
		return c.Value
	}
	return s.opt.LocalClusterName
}

// handleCluster records the choice and returns to where you were.
//
// A cookie, like the rail and the scope, and for the same reason: it has to
// survive following a link. It also CLEARS the scope, because a customer is a
// namespace in one cluster and the same name in another is somebody else — or
// nobody. Carrying it across would show an empty screen for a customer that
// exists, which reads as data having disappeared.
func (s *Server) handleCluster(w http.ResponseWriter, r *http.Request, _ Identity) {
	to := r.URL.Query().Get("to")
	if to == "" {
		to = r.FormValue("to")
	}

	known := to == s.opt.LocalClusterName || (to == allClusters && s.Federated())
	if _, ok := s.clusters[to]; ok {
		known = true
	}
	if !known {
		to = s.opt.LocalClusterName
	}

	http.SetCookie(w, &http.Cookie{
		Name: clusterCookie, Value: to, Path: "/", HttpOnly: true,
		Secure: r.TLS != nil, SameSite: http.SameSiteLaxMode, MaxAge: 30 * 24 * 3600,
	})
	http.SetCookie(w, &http.Cookie{
		Name: scopeCookie, Value: "", Path: "/", HttpOnly: true,
		Secure: r.TLS != nil, SameSite: http.SameSiteLaxMode, MaxAge: -1,
	})

	back := r.URL.Query().Get("back")
	if back == "" || back[0] != '/' || (len(back) > 1 && back[1] == '/') {
		back = "/"
	}
	http.Redirect(w, r, back, http.StatusSeeOther)
}

// clusterChoice is one entry in the picker.
type clusterChoice struct {
	Name string
	// Label is what it is called on screen, which for the everywhere entry is not
	// its value.
	Label string
	On    bool
}

// clusterChoices is what the picker offers, local first.
func (s *Server) clusterChoices(r *http.Request) []clusterChoice {
	if !s.Federated() {
		return nil
	}
	current := s.clusterOf(r)
	out := []clusterChoice{
		// Everywhere first: it is the view somebody wants when they do not
		// already know which cluster the problem is in, which is most mornings.
		{Name: allClusters, Label: "Everywhere", On: current == allClusters},
		{Name: s.opt.LocalClusterName, Label: s.opt.LocalClusterName,
			On: current == s.opt.LocalClusterName},
	}
	rest := make([]string, 0, len(s.clusters))
	for name := range s.clusters {
		rest = append(rest, name)
	}
	sort.Strings(rest)
	for _, name := range rest {
		out = append(out, clusterChoice{Name: name, Label: name, On: current == name})
	}
	return out
}

// Everywhere reports whether this request is about every cluster at once.
func (s *Server) Everywhere(r *http.Request) bool {
	return s.Federated() && s.clusterOf(r) == allClusters
}

// askedCluster is the cluster a page should use when it can only use one.
//
// "Everywhere" collapses to the local cluster for pages about a single object,
// which never see it: a link out of an aggregated list carries its own cluster,
// so this is the fallback for somebody typing a URL by hand.
func (s *Server) askedCluster(r *http.Request) string {
	if c := s.clusterOf(r); c != allClusters {
		return c
	}
	return s.opt.LocalClusterName
}
