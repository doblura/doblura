// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Antoni Romera

package console

import (
	"context"
	"sort"
	"sync"
)

// Asking every cluster at once.
//
// The picker chooses one cluster; this is the other half — the view that shows
// what is happening everywhere, which is the question somebody actually has at
// nine in the morning.
//
// One property matters more than the rest of this file: **a cluster that could
// not be asked is SAID, never dropped.** A federated list that silently returns
// fewer rows when a cluster is unreachable is a page that reports an outage as
// calm. It is the same failure as a status page that says "load: normal" on the
// strength of no measurement, and the same failure as retention applying a policy
// to a listing it could not refresh — three times in this project now, which is
// why it has its own type here rather than a convention.

// clusterResult is what one cluster answered, or why it did not.
type clusterResult[T any] struct {
	Cluster string
	Value   T
	// Err is why this cluster is not represented. Kept as the API server's own
	// words: a 403 from a remote cluster names the verb, the resource and the
	// person, which is what somebody forwards to whoever administers it.
	Err error
}

// fanOut asks every cluster the same question, as the same person.
//
// Concurrently, because the slowest cluster would otherwise set the page's load
// time and one unreachable cluster would make every page wait for its timeout.
// The results come back in a stable order regardless of who answered first: a
// list that reorders itself between refreshes is one nobody can read.
func fanOut[T any](
	ctx context.Context,
	s *Server,
	id Identity,
	ask func(ctx context.Context, id Identity) (T, error),
) []clusterResult[T] {
	names := s.clusterNames()
	out := make([]clusterResult[T], len(names))

	var wg sync.WaitGroup
	for i, name := range names {
		wg.Add(1)
		go func(i int, name string) {
			defer wg.Done()
			// A copy per goroutine. Identity carries WHERE, so sharing one would
			// have every cluster answering about whichever name was written last
			// — a data mix-up across customers that no test would catch.
			who := id
			who.Cluster = name
			v, err := ask(ctx, who)
			out[i] = clusterResult[T]{Cluster: name, Value: v, Err: err}
		}(i, name)
	}
	wg.Wait()

	sort.SliceStable(out, func(a, b int) bool {
		// Local first, then alphabetical — the same order as the picker, so the
		// two agree on screen.
		if out[a].Cluster == s.opt.LocalClusterName {
			return true
		}
		if out[b].Cluster == s.opt.LocalClusterName {
			return false
		}
		return out[a].Cluster < out[b].Cluster
	})
	return out
}

// clusterTrouble is a cluster that did not answer, for the page to show.
type clusterTrouble struct {
	Cluster string
	Why     string
}

// troubles collects the clusters that could not be asked.
func troubles[T any](results []clusterResult[T]) []clusterTrouble {
	var out []clusterTrouble
	for _, r := range results {
		if r.Err != nil {
			out = append(out, clusterTrouble{Cluster: r.Cluster, Why: r.Err.Error()})
		}
	}
	return out
}
