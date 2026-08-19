// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Antoni Romera

package console

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"k8s.io/client-go/rest"
)

func federated() *Server {
	return &Server{
		cfg: &rest.Config{Host: "https://local.invalid"},
		opt: Options{LocalClusterName: "principal"},
		clusters: map[string]*rest.Config{
			"remoto": {Host: "https://remoto.invalid"},
		},
	}
}

// A cluster nobody knows is refused, never quietly answered locally.
//
// The worst thing this code could do is show one cluster's data under another
// cluster's name: somebody looks at a screen that says "remoto", reads that an
// environment is up, and it is a different environment in a different place.
func TestAnUnknownClusterIsRefusedRatherThanAnsweredLocally(t *testing.T) {
	s := federated()

	if _, err := s.configFor("inventado"); err == nil {
		t.Fatal("an unknown cluster fell back to the local one, so the screen would " +
			"show local data under a remote cluster's name")
	} else if !strings.Contains(err.Error(), "remoto") {
		t.Errorf("the refusal does not say which clusters exist: %v", err)
	}

	// The local one, by name and by absence.
	for _, name := range []string{"", "principal"} {
		cfg, err := s.configFor(name)
		if err != nil {
			t.Fatalf("configFor(%q) refused the local cluster: %v", name, err)
		}
		if cfg.Host != "https://local.invalid" {
			t.Errorf("configFor(%q) returned %s", name, cfg.Host)
		}
	}

	cfg, err := s.configFor("remoto")
	if err != nil || cfg.Host != "https://remoto.invalid" {
		t.Fatalf("the remote cluster resolved to %v, %v", cfg, err)
	}
}

// A cookie naming a cluster that no longer exists lands on the local one.
//
// Removing a cluster must not leave everybody who was looking at it on an error
// page they cannot get out of without clearing cookies.
func TestAStaleClusterCookieFallsBackToLocal(t *testing.T) {
	s := federated()
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.AddCookie(&http.Cookie{Name: clusterCookie, Value: "el-que-se-fue"})

	if got := s.clusterOf(r); got != "principal" {
		t.Fatalf("a stale cluster cookie resolved to %q", got)
	}
}

// One cluster changes nothing about the interface.
//
// Decision 3: the open project stays completely usable on its own, with no
// degraded mode and no reminder that a paid thing exists.
func TestASingleClusterInstallSeesNoPicker(t *testing.T) {
	s := &Server{
		cfg:      &rest.Config{Host: "https://local.invalid"},
		opt:      Options{LocalClusterName: "local"},
		clusters: map[string]*rest.Config{},
	}
	if s.Federated() {
		t.Fatal("a single-cluster install reports itself as federated")
	}
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	if choices := s.clusterChoices(r); choices != nil {
		t.Fatalf("a single-cluster install offers a cluster picker: %v", choices)
	}
}

// Choosing a cluster clears the customer scope.
//
// A customer is a namespace in one cluster; the same name in another is somebody
// else, or nobody. Carrying the scope across would show an empty screen for a
// customer that exists, which reads as data having disappeared.
func TestChoosingAClusterClearsTheScope(t *testing.T) {
	s := federated()
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/cluster?to=remoto&back=/customers", nil)

	s.handleCluster(w, r, Identity{User: "toni"})

	var clearedScope, setCluster bool
	for _, c := range w.Result().Cookies() {
		switch c.Name {
		case scopeCookie:
			clearedScope = c.MaxAge < 0 || c.Value == ""
		case clusterCookie:
			setCluster = c.Value == "remoto"
		}
	}
	if !setCluster {
		t.Error("the cluster was not recorded")
	}
	if !clearedScope {
		t.Error("the customer scope survived a cluster change, so the next page " +
			"asks another cluster about a customer that is not in it")
	}
	if w.Code != http.StatusSeeOther {
		t.Errorf("did not redirect back: %d", w.Code)
	}
}
