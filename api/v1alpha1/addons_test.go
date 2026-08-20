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

// Modules have to be for the Odoo that is going to run them.
//
// The failure this prevents is invisible until the last possible moment and looks
// like something else: cloning an OCA repository at 17.0 onto an Odoo 18 image is
// accepted by git, by buildah, by the registry and by the image study — which
// counted the modules and reported three more than the base — and then Odoo says
// "Module mis_builder: invalid manifest". That sentence does not contain the word
// version, and it arrives after the expensive part.
func TestModulesOnTheWrongOdooAreNamed(t *testing.T) {
	repos := []AddonRepo{
		{Name: "mis-builder", Ref: "17.0"},
		{Name: "server-tools", Ref: "18.0"},
		{Name: "ours", Ref: "3f9a1c2"}, // a commit says nothing about a series
		{Name: "theirs", Ref: "main"},  // nor does a branch name
	}

	wrong := AddonsOnTheWrongSeries("18.0", repos)
	if len(wrong) != 1 || !strings.Contains(wrong[0], "mis-builder") {
		t.Fatalf("expected only mis-builder to be named: %v", wrong)
	}

	// It only speaks when it knows. A rule that guesses is a rule people turn off.
	if got := AddonsOnTheWrongSeries("", repos); got != nil {
		t.Errorf("with no version to compare against it invented findings: %v", got)
	}
	if got := AddonsOnTheWrongSeries("17.0", []AddonRepo{{Name: "a", Ref: "17.0"}}); got != nil {
		t.Errorf("a matching series was reported as wrong: %v", got)
	}
	// "18" and "18.0" are the same major.
	if got := AddonsOnTheWrongSeries("18", []AddonRepo{{Name: "a", Ref: "18.0"}}); got != nil {
		t.Errorf("18 and 18.0 were treated as different: %v", got)
	}
	// And a two-digit series is not confused with a one-digit one.
	if got := SeriesOf("9.0"); got != "9" {
		t.Errorf("SeriesOf(9.0) = %q", got)
	}
	if got := SeriesOf("18.0-fix"); got != "" {
		t.Errorf("a decorated branch name was read as a series: %q", got)
	}
}
