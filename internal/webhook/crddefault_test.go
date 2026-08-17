// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Antoni Romera

package webhook

import (
	"os"
	"testing"

	"gopkg.in/yaml.v3"

	doblurav1alpha1 "github.com/doblura/doblura/api/v1alpha1"
)

// The quota's default exists in two places that cannot reference each other: the
// `+kubebuilder:default=3` marker the API server applies, and the Go constant this
// webhook falls back to. A drift between them would be invisible — the cluster
// would enforce one number and anything reading the object in Go would use the
// other — so it is a test, and the test parses the generated CRD rather than
// grepping it.
func TestTheTenantQuotaDefaultMatchesTheCRD(t *testing.T) {
	raw, err := os.ReadFile("../../config/crd/doblura.dev_odootenants.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var crd struct {
		Spec struct {
			Versions []struct {
				Name   string `yaml:"name"`
				Schema struct {
					OpenAPIV3Schema struct {
						Properties struct {
							Spec struct {
								Properties struct {
									MaxEphemeralEnvironments struct {
										Default *int32 `yaml:"default"`
										Type    string `yaml:"type"`
										Minimum *int32 `yaml:"minimum"`
									} `yaml:"maxEphemeralEnvironments"`
								} `yaml:"properties"`
							} `yaml:"spec"`
						} `yaml:"properties"`
					} `yaml:"openAPIV3Schema"`
				} `yaml:"schema"`
			} `yaml:"versions"`
		} `yaml:"spec"`
	}
	if err := yaml.Unmarshal(raw, &crd); err != nil {
		t.Fatal(err)
	}

	var found bool
	for _, v := range crd.Spec.Versions {
		if v.Name != "v1alpha1" {
			continue
		}
		found = true
		field := v.Schema.OpenAPIV3Schema.Properties.Spec.Properties.MaxEphemeralEnvironments
		if field.Default == nil {
			t.Fatal("maxEphemeralEnvironments has no default in the CRD: a tenant created without it " +
				"would get no quota at all in the API server")
		}
		if *field.Default != doblurav1alpha1.DefaultMaxEphemeralEnvironments {
			t.Errorf("the CRD defaults maxEphemeralEnvironments to %d and Go falls back to %d",
				*field.Default, doblurav1alpha1.DefaultMaxEphemeralEnvironments)
		}
		// Minimum 0 rather than 1: zero is a real answer, and this is where the
		// API server enforces that it cannot be negative.
		if field.Minimum == nil || *field.Minimum != 0 {
			t.Errorf("maxEphemeralEnvironments should have minimum 0; got %v", field.Minimum)
		}
	}
	if !found {
		t.Fatal("no v1alpha1 version in the generated OdooTenant CRD")
	}
}
