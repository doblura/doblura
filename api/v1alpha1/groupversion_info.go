// SPDX-License-Identifier: Apache-2.0
// Copyright (C) 2026 Antoni Romera

// Package v1alpha1 contains the Doblura API.
//
// v1alpha1: the contract may change. Once it settles, v1beta1 with conversion,
// not a silent rename.
//
// +kubebuilder:object:generate=true
// +groupName=doblura.dev
package v1alpha1

import (
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/scheme"
)

var (
	// GroupVersion is the group/version of these types.
	GroupVersion = schema.GroupVersion{Group: "doblura.dev", Version: "v1alpha1"}

	// SchemeBuilder registers the Go types into a Scheme.
	SchemeBuilder = &scheme.Builder{GroupVersion: GroupVersion}

	// AddToScheme adds this group-version's types to a Scheme.
	AddToScheme = SchemeBuilder.AddToScheme
)
