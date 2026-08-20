// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Antoni Romera

package metrics

import (
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
)

// Nothing is labelled by environment name.
//
// A review environment exists per pull request, so a busy month is thousands of
// names that will never be seen again — and every distinct label combination is a
// time series held in memory for the whole retention. This is a Prometheus outage
// with a delay on it, and it is the single easiest mistake to make in a file like
// this one.
func TestNothingIsLabelledBySomethingUnbounded(t *testing.T) {
	forbidden := []string{"environment", "env", "name", "pod", "job", "namespace_name"}
	for _, c := range []prometheus.Collector{PhaseDuration, PhaseFailures, CustomerRelease} {
		desc := make(chan *prometheus.Desc, 4)
		c.Describe(desc)
		close(desc)
		for d := range desc {
			s := d.String()
			for _, f := range forbidden {
				if strings.Contains(s, `"`+f+`"`) {
					t.Errorf("%s is labelled by %q, which is unbounded", s, f)
				}
			}
		}
	}
}

// The buckets cover a real migration.
//
// The client library's defaults stop at ten seconds, a scale on which every Odoo
// migration lands in +Inf and the histogram answers nothing at all.
func TestTheBucketsCoverARealMigration(t *testing.T) {
	PhaseDuration.WithLabelValues("migrate", "Production", "acme").Observe(900)

	var m dto.Metric
	if err := PhaseDuration.WithLabelValues("migrate", "Production", "acme").(prometheus.Metric).Write(&m); err != nil {
		t.Fatal(err)
	}
	var above, below uint64
	for _, b := range m.GetHistogram().GetBucket() {
		if b.GetUpperBound() >= 900 {
			above++
		} else {
			below++
		}
	}
	if above < 3 {
		t.Error("a fifteen-minute migration is near the top of the range: the buckets " +
			"cannot distinguish a slow migration from a stuck one")
	}
	if below < 3 {
		t.Error("there is nothing below a fifteen-minute migration to compare against")
	}
}

// A customer on a new release stops being reported on the old one.
func TestACustomerIsOnOneReleaseAtATime(t *testing.T) {
	CustomerRelease.Reset()
	CustomerRelease.WithLabelValues("acme", "2026.2").Set(1)
	CustomerRelease.DeletePartialMatch(prometheus.Labels{"tenant": "acme"})
	CustomerRelease.WithLabelValues("acme", "2026.3").Set(1)

	got := make(chan prometheus.Metric, 8)
	CustomerRelease.Collect(got)
	close(got)
	n := 0
	for range got {
		n++
	}
	if n != 1 {
		t.Errorf("a customer is reported on %d releases at once, so counting who is "+
			"on the new one counts them twice", n)
	}
}
