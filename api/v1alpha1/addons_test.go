// SPDX-License-Identifier: Apache-2.0
// Copyright (C) 2026 Antoni Romera

package v1alpha1

import (
	"reflect"
	"strings"
	"testing"
)

// The order of the addons path decides which code runs. In Odoo the first entry
// wins, so this is not cosmetic.
func TestAddonsPathOrdering(t *testing.T) {
	spec := AddonsSpec{
		Baked: []string{"/opt/odoo/addons-custom"},
		Repos: []AddonRepo{
			{Name: "oca-account", URL: "https://github.com/OCA/account-financial-tools", Ref: "17.0"},
			{Name: "propios", URL: "git@github.com:acme/addons", Ref: "abc123", Paths: []string{"17.0", "shared"}},
		},
		Volume: &AddonVolume{ClaimName: "agg", Paths: []string{"/extra"}},
	}

	t.Run("ExternalFirst is the default and external sources come first", func(t *testing.T) {
		spec.Precedence = PrecedenceExternalFirst
		got := spec.AddonsPathFor(AddonRepoMountBase)
		want := []string{
			AddonRepoMountBase + "/oca-account",
			AddonRepoMountBase + "/propios/17.0",
			AddonRepoMountBase + "/propios/shared",
			AddonVolumeMountPath + "/extra",
			"/opt/odoo/addons-custom",
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("\n  got:      %v\n  expected: %v", got, want)
		}
	})

	t.Run("BakedFirst inverts the order", func(t *testing.T) {
		spec.Precedence = PrecedenceBakedFirst
		got := spec.AddonsPathFor(AddonRepoMountBase)
		if got[0] != "/opt/odoo/addons-custom" {
			t.Fatalf("with BakedFirst the image comes first, got %q", got[0])
		}
	})
}

// Baked only: the simplest case must work without declaring anything else.
func TestAddonsBakedOnly(t *testing.T) {
	spec := AddonsSpec{Baked: []string{"/a", "/b"}}
	got := spec.AddonsPathFor(AddonRepoMountBase)
	if !reflect.DeepEqual(got, []string{"/a", "/b"}) {
		t.Fatalf("got %v", got)
	}
}

// With nothing declared the addons path is empty: the image's own path is used,
// and the operator does not invent paths that do not exist.
func TestAddonsEmpty(t *testing.T) {
	spec := AddonsSpec{}
	if got := spec.AddonsPathFor(AddonRepoMountBase); len(got) != 0 {
		t.Fatalf("expected empty, got %v", got)
	}
}

// A repo without paths points at its root, which is the OCA case.
func TestRepoWithoutPathsUsesTheRoot(t *testing.T) {
	spec := AddonsSpec{Repos: []AddonRepo{{Name: "r", URL: "u"}}}
	got := spec.AddonsPathFor("/base")
	if len(got) != 1 || got[0] != "/base/r" {
		t.Fatalf("got %v", got)
	}
}

// The acknowledgement has to be awkward to type, on purpose.
func TestAcknowledgementIsExplicit(t *testing.T) {
	if len(AcknowledgementUnsafe) < 30 || !strings.Contains(AcknowledgementUnsafe, "charge") {
		t.Error("the acknowledgement must be long and must name the real consequence")
	}
}
