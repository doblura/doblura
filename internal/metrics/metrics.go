// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Antoni Romera

// Package metrics is what doblura tells Prometheus about itself.
//
// Two decisions shape everything here, and both are about what NOT to do.
//
// CARDINALITY. A label with an unbounded set of values is a Prometheus outage
// with a delay on it: every distinct combination is a time series kept in memory
// for as long as the retention says. The environment NAME is exactly such a
// label — a review environment exists per pull request, so a busy month is
// thousands of names that will never be seen again. So nothing here is labelled
// by environment. Tenant is bounded by how many customers an installation has,
// which is a number its operator chose; purpose and phase are enums.
//
// THE RELEASE IS AN INFO METRIC, not a label on the durations. Putting the
// release version on a duration histogram splits a customer's history in two
// every time they move onto a new one — and "did migrations get slower after
// 2026.3?" is precisely the question that needs both halves in one series. A
// separate gauge carrying the version is joined at query time:
//
//	histogram_quantile(0.9,
//	  sum by (le, version) (
//	    rate(doblura_phase_duration_seconds_bucket{phase="migrate"}[1d])
//	    * on (tenant) group_left(version) doblura_customer_release))
package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
)

var (
	// PhaseDuration is how long each phase of bringing up an environment took.
	//
	// The buckets are not the client library's defaults, which stop at ten
	// seconds — a scale on which every Odoo migration is "+Inf" and the histogram
	// answers nothing. These run from half a minute to two hours, which is the
	// range a real migration lives in, and the top bucket is deliberately far
	// enough out that hitting it means something is wrong rather than that the
	// database is large.
	PhaseDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name: "doblura_phase_duration_seconds",
		Help: "How long a phase of an environment took, from the Job's own start and completion times.",
		Buckets: []float64{
			30, 60, 120, 300, 600, 1200, 2400, 3600, 7200,
		},
	}, []string{"phase", "purpose", "tenant"})

	// PhaseFailures counts phases that ended badly.
	//
	// A counter and not a gauge: the question is "how often", and a gauge of the
	// current number of failed things answers "right now", which is what the
	// objects already say better.
	PhaseFailures = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "doblura_phase_failures_total",
		Help: "Phases that failed, by phase and customer.",
	}, []string{"phase", "purpose", "tenant"})

	// CustomerRelease is which version of your own product each customer is on.
	//
	// The value is always 1; the information is in the labels. That is the
	// standard shape for a fact rather than a measurement, and it is what makes
	// the durations above joinable to a release without being split by it.
	CustomerRelease = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "doblura_customer_release",
		Help: "1 for each customer, labelled with the product release they are on.",
	}, []string{"tenant", "version"})
)

func init() {
	metrics.Registry.MustRegister(PhaseDuration, PhaseFailures, CustomerRelease)
}
