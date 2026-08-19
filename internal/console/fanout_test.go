// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Antoni Romera

package console

import (
	"context"
	"errors"
	"strings"
	"testing"

	"k8s.io/client-go/rest"
)

// A cluster that could not be asked is SAID, never dropped.
//
// A federated list that quietly returns fewer rows when one cluster is
// unreachable reports an outage as calm. It is the third time this project has
// had to learn it: a status page saying "load: normal" on the strength of no
// measurement, retention applying a policy to a listing it could not refresh, and
// this.
func TestAClusterThatDidNotAnswerIsNeverSilentlyDropped(t *testing.T) {
	s := &Server{
		cfg:      &rest.Config{Host: "https://local.invalid"},
		opt:      Options{LocalClusterName: "principal"},
		clusters: map[string]*rest.Config{"remoto": {Host: "https://remoto.invalid"}},
	}

	results := fanOut(context.Background(), s, Identity{User: "toni"},
		func(_ context.Context, who Identity) (objectsView, error) {
			if who.Cluster == "remoto" {
				return objectsView{}, errors.New("connection refused")
			}
			return objectsView{Rows: []objectRow{{Name: "uno", Href: "/e/demo/uno"}}}, nil
		})

	merged := mergeObjects(&objectKind{Slug: "environments"}, results)

	if len(merged.Troubles) != 1 || merged.Troubles[0].Cluster != "remoto" {
		t.Fatalf("the unreachable cluster is not reported: %+v", merged.Troubles)
	}
	if !strings.Contains(merged.Troubles[0].Why, "connection refused") {
		t.Errorf("the reason was replaced with something vaguer: %q", merged.Troubles[0].Why)
	}
	if len(merged.Rows) != 1 {
		t.Fatalf("the rows from the cluster that DID answer were lost: %d", len(merged.Rows))
	}

	// The link has to carry its cluster, or following it asks the wrong API
	// server about an object that is not there.
	if !strings.Contains(merged.Rows[0].Href, "to=principal") ||
		!strings.Contains(merged.Rows[0].Href, "%2Fe%2Fdemo%2Funo") {
		t.Errorf("the row's link does not carry its cluster: %q", merged.Rows[0].Href)
	}
	if merged.Rows[0].Cluster != "principal" {
		t.Errorf("the row does not say where it is: %q", merged.Rows[0].Cluster)
	}
}

// Every cluster is asked as the same person, and never as each other.
//
// Identity carries WHERE, so a fan-out sharing one copy would have every cluster
// answering about whichever name was written last — a mix-up across customers
// that no ordinary test would catch.
func TestEachClusterIsAskedAboutItself(t *testing.T) {
	s := &Server{
		cfg:      &rest.Config{Host: "https://local.invalid"},
		opt:      Options{LocalClusterName: "principal"},
		clusters: map[string]*rest.Config{"a": {}, "b": {}, "c": {}},
	}

	results := fanOut(context.Background(), s, Identity{User: "ana"},
		func(_ context.Context, who Identity) (string, error) {
			if who.User != "ana" {
				return "", errors.New("the person changed between clusters")
			}
			return who.Cluster, nil
		})

	for _, r := range results {
		if r.Err != nil {
			t.Fatalf("%s: %v", r.Cluster, r.Err)
		}
		if r.Value != r.Cluster {
			t.Errorf("%s was asked about %q", r.Cluster, r.Value)
		}
	}
	// Stable order, local first: a list that reorders between refreshes is one
	// nobody can read.
	if results[0].Cluster != "principal" {
		t.Errorf("the local cluster is not first: %s", results[0].Cluster)
	}
}
