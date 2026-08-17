// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Antoni Romera

package controller

import (
	"time"

	batchv1 "k8s.io/api/batch/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	doblurav1alpha1 "github.com/doblura/doblura/api/v1alpha1"
)

func mustDur(s string) time.Duration {
	d, err := time.ParseDuration(s)
	if err != nil {
		panic(err)
	}
	return d
}

// jobRunFor fabricates a Job that appears to have taken d.
func jobRunFor(d string) *batchv1.Job {
	dur := mustDur(d)
	start := metav1.NewTime(time.Now().Add(-dur))
	end := metav1.NewTime(start.Add(dur))
	return &batchv1.Job{Status: batchv1.JobStatus{
		StartTime:      &start,
		CompletionTime: &end,
		Succeeded:      1,
	}}
}

func findCond(st *doblurav1alpha1.OdooRehearsalStatus, t string) *metav1.Condition {
	for i := range st.Conditions {
		if st.Conditions[i].Type == t {
			return &st.Conditions[i]
		}
	}
	return nil
}
