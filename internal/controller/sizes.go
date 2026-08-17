// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Antoni Romera

package controller

import (
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	doblurav1alpha1 "github.com/doblura/doblura/api/v1alpha1"
)

// The intent-to-resources translation table.
//
// It lives here rather than in the CRD on purpose: the user declares "large"
// because their database is large, not because they know how much RAM the `-u`
// needs. The day we find out that large needs 12Gi instead of 8, it changes here
// and nobody has to edit their manifests.
//
// The numbers come from an Odoo reality: the migration is a single-threaded
// Python process that loads records into memory. It scales with database size,
// not with user count.
func sizeToResources(s doblurav1alpha1.Size) corev1.ResourceRequirements {
	type spec struct{ cpuReq, memReq, memLim string }
	table := map[doblurav1alpha1.Size]spec{
		doblurav1alpha1.SizeSmall:  {"200m", "1Gi", "2Gi"},
		doblurav1alpha1.SizeMedium: {"500m", "2Gi", "4Gi"},
		doblurav1alpha1.SizeLarge:  {"1", "4Gi", "12Gi"},
	}
	v, ok := table[s]
	if !ok {
		v = table[doblurav1alpha1.SizeMedium]
	}
	return corev1.ResourceRequirements{
		Requests: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse(v.cpuReq),
			corev1.ResourceMemory: resource.MustParse(v.memReq),
		},
		// No CPU limit on purpose: the migration is CPU-bound and single-
		// threaded. Throttling it only makes it longer, and the duration is
		// precisely what we are measuring.
		Limits: corev1.ResourceList{
			corev1.ResourceMemory: resource.MustParse(v.memLim),
		},
	}
}
