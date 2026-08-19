// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Antoni Romera

package controller

import (
	"testing"

	doblurav1alpha1 "github.com/doblura/doblura/api/v1alpha1"
)

// Long polling reaches the gevent worker on every Odoo version.
//
// Odoo renamed the path — /longpolling/ up to 15, /websocket from 16 — and only
// the newer one was routed to port 8072. On a 14 or a 15 that sent every long-poll
// request to the ordinary HTTP workers, where each one sits holding a worker open;
// with workers = 2, two open chat tabs are the entire environment. It looks like an
// Odoo that works until somebody opens Discuss, which is about the hardest kind of
// fault to attribute.
func TestBothLongPollPathsReachTheGeventWorker(t *testing.T) {
	env := &doblurav1alpha1.OdooEnvironment{}
	env.Name = "acme-prod"

	paths := ingressPaths(env)

	port := map[string]int32{}
	for _, p := range paths {
		if p.Backend.Service == nil {
			t.Fatalf("path %s has no service backend", p.Path)
		}
		port[p.Path] = p.Backend.Service.Port.Number
	}

	for _, p := range []string{"/websocket", "/longpolling/"} {
		got, ok := port[p]
		if !ok {
			t.Errorf("%s is not routed at all; on the Odoo versions that use it, "+
				"long polling lands on the HTTP workers and holds them open", p)
			continue
		}
		if got != 8072 {
			t.Errorf("%s goes to port %d, not the gevent worker on 8072", p, got)
		}
	}
	if port["/"] != 80 {
		t.Errorf("everything else goes to port %d, not 80", port["/"])
	}

	// Order matters to some ingress controllers, which take the first match
	// rather than the longest. The catch-all must not come first.
	if paths[len(paths)-1].Path != "/" {
		t.Fatalf("the catch-all is not last (%s is), so a controller that takes "+
			"the first match sends websockets to the HTTP workers",
			paths[len(paths)-1].Path)
	}
}
