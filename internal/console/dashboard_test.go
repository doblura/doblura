// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Antoni Romera

package console

import (
	"errors"
	"testing"
)

// Totals add up, and never quietly leave a cluster out.
//
// A number is the one thing on this page nobody checks: four clusters answering
// and a fifth being unreachable produces a total that looks exactly like a total.
func TestTheOverviewTotalsSayWhatTheyDoNotInclude(t *testing.T) {
	merged := mergeDashboards([]clusterResult[dashboardView]{
		{Cluster: "principal", Value: dashboardView{
			Customers: 2, Environments: 5, Up: 4, WorkloadVisible: true,
			Attention: []attentionRow{{Name: "roto", Href: "/e/demo/roto", severity: 0}},
			Quotas:    []quotaRow{{Customer: "acme", Href: "/c/demo/acme"}},
		}},
		{Cluster: "remoto", Value: dashboardView{
			Customers: 1, Environments: 1, Up: 1, WorkloadVisible: true,
		}},
		{Cluster: "caido", Err: errors.New("connection refused")},
	})

	if merged.Customers != 3 || merged.Environments != 6 || merged.Up != 5 {
		t.Fatalf("the totals are %d customers, %d environments, %d up",
			merged.Customers, merged.Environments, merged.Up)
	}
	if len(merged.Troubles) != 1 || merged.Troubles[0].Cluster != "caido" {
		t.Fatalf("the unreachable cluster is not named: %+v", merged.Troubles)
	}

	// Rows carry where they are, and their links go there.
	if merged.Attention[0].Cluster != "principal" {
		t.Errorf("an attention row does not say which cluster it is in")
	}
	if got := merged.Attention[0].Href; got == "/e/demo/roto" {
		t.Errorf("the link does not switch cluster, so following it asks the "+
			"wrong API server: %q", got)
	}
	if merged.Quotas[0].Cluster != "principal" {
		t.Errorf("a quota row does not say which cluster it is in")
	}
}

// One cluster that cannot see workloads makes the "up" count meaningless
// everywhere, not partly meaningless.
//
// A total that counts four clusters and guesses the fifth is worse than one that
// says it cannot tell: the first is believed.
func TestOneBlindClusterMakesTheUpCountUnavailable(t *testing.T) {
	merged := mergeDashboards([]clusterResult[dashboardView]{
		{Cluster: "principal", Value: dashboardView{Up: 4, WorkloadVisible: true}},
		{Cluster: "remoto", Value: dashboardView{Up: 0, WorkloadVisible: false}},
	})
	if merged.WorkloadVisible {
		t.Fatal("the overview claims to know how many are up while one cluster " +
			"could not be asked about its workloads")
	}
}
