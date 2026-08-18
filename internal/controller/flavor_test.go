// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Antoni Romera

package controller

import (
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"

	doblurav1alpha1 "github.com/doblura/doblura/api/v1alpha1"
)

// The flavour differences, pinned.
//
// These are unit tests rather than an end-to-end run, and the reason is worth
// recording: the published Doodba base ships the scaffolding and NOT Odoo, so a
// genuine Doodba image cannot be assembled in a few minutes. What was verified
// against the real ghcr.io/tecnativa/doodba:18.0 is in the comment on ImageFlavor
// — the uid, the cmd, the paths, the absence of an odoo user. What is verified
// here is that doblura acts on those facts.

func flavourEnv(f doblurav1alpha1.ImageFlavor) *doblurav1alpha1.OdooEnvironment {
	return &doblurav1alpha1.OdooEnvironment{
		Spec: doblurav1alpha1.OdooEnvironmentSpec{
			Image:       "example:18",
			ImageFlavor: f,
			Database:    doblurav1alpha1.DatabaseSpec{Host: "h", User: "u", PasswordSecret: "s"},
		},
	}
}

func TestDoodbaAddonsPathIsAddedAndOfficialsIsNot(t *testing.T) {
	doodba := envOdooConf(flavourEnv(doblurav1alpha1.FlavorDoodba))
	if !strings.Contains(doodba, "addons_path = "+doblurav1alpha1.DoodbaAddonsPath) {
		t.Errorf("a Doodba environment must load %s; got:\n%s",
			doblurav1alpha1.DoodbaAddonsPath, doodba)
	}

	// The official image finds its own addons, and writing an addons_path that
	// does NOT include them replaces the default rather than adding to it —
	// which is how an OpenUpgrade run once became unloadable.
	official := envOdooConf(flavourEnv(doblurav1alpha1.FlavorOfficial))
	if strings.Contains(official, "addons_path") {
		t.Errorf("an official image must be left to find its own addons; got:\n%s", official)
	}
}

func TestDoodbaOverridesTheCommand(t *testing.T) {
	// Doodba's cmd is `python3`. Without an explicit command, doblura's arguments
	// become `python3 -c /etc/doblura/odoo.conf`, which hands an ini file to the
	// Python interpreter as a program.
	pod := envServingPod(flavourEnv(doblurav1alpha1.FlavorDoodba))
	got := pod.Spec.Containers[0].Command
	if len(got) != 1 || got[0] != "odoo" {
		t.Errorf("a Doodba serving pod must run odoo explicitly, got %v", got)
	}

	official := envServingPod(flavourEnv(doblurav1alpha1.FlavorOfficial))
	if cmd := official.Spec.Containers[0].Command; len(cmd) != 0 {
		t.Errorf("the official image's own entrypoint must be left alone, got %v", cmd)
	}
}

func TestCronTierGetsTheSameCommandAsTheWebTier(t *testing.T) {
	// Easy to forget, and the failure is a cron tier that crash-loops while the
	// web tier is fine — which reads as a cron problem and is not one.
	env := flavourEnv(doblurav1alpha1.FlavorDoodba)
	env.Spec.Workload = &doblurav1alpha1.WorkloadSplit{
		Cron: &doblurav1alpha1.CronTier{Replicas: 1, Threads: 2},
	}
	cron := envCronPod(env)
	if got := cron.Spec.Containers[0].Command; len(got) != 1 || got[0] != "odoo" {
		t.Errorf("the cron tier must get the flavour's command too, got %v", got)
	}
}

func TestPreflightChecksWhatTheFlavourPromised(t *testing.T) {
	doodba := envPreflight(flavourEnv(doblurav1alpha1.FlavorDoodba))
	for _, want := range []string{
		"command -v odoo",
		doblurav1alpha1.DoodbaAddonsPath,
		doblurav1alpha1.DataDirPath,
	} {
		if !strings.Contains(doodba, want) {
			t.Errorf("the Doodba preflight must check %q; got:\n%s", want, doodba)
		}
	}

	// The official preflight must NOT demand Doodba's directory: failing on a
	// path the flavour never promised is a check that invents a requirement.
	official := envPreflight(flavourEnv(doblurav1alpha1.FlavorOfficial))
	if strings.Contains(official, doblurav1alpha1.DoodbaAddonsPath) {
		t.Errorf("an official image must not be asked for Doodba's layout; got:\n%s", official)
	}
}

// The module update belongs to the web tier alone.
//
// Two pods running click-odoo-update against one database at the same time is
// the double execution the cron split exists to prevent, applied to the one
// operation that rewrites the schema. Easy to reintroduce by adding the update
// to a shared helper, which is exactly how it got in the first time.
func TestOnlyTheWebTierUpdatesModules(t *testing.T) {
	env := flavourEnv(doblurav1alpha1.FlavorOfficial)
	env.Spec.Purpose = doblurav1alpha1.PurposeReview
	env.Spec.Workload = &doblurav1alpha1.WorkloadSplit{
		Cron: &doblurav1alpha1.CronTier{Replicas: 1, Threads: 2},
	}

	if !hasInit(envServingPod(env).Spec.InitContainers, "update") {
		t.Error("a Review environment must bring its modules level as it starts")
	}
	if hasInit(envCronPod(env).Spec.InitContainers, "update") {
		t.Error("the cron tier must not update: that is two updates against one database")
	}
}

// Production does not update itself because a node was drained.
func TestPurposeDecidesWhetherModulesUpdateOnStart(t *testing.T) {
	for _, tc := range []struct {
		purpose doblurav1alpha1.EnvPurpose
		want    bool
	}{
		{doblurav1alpha1.PurposeReview, true},
		{doblurav1alpha1.PurposeQA, true},
		{doblurav1alpha1.PurposeStaging, false},
		{doblurav1alpha1.PurposeProduction, false},
		{"", false},
	} {
		env := flavourEnv(doblurav1alpha1.FlavorOfficial)
		env.Spec.Purpose = tc.purpose
		if got := env.Spec.UpdatesOnStart(); got != tc.want {
			t.Errorf("purpose %q: updates on start = %v, want %v", tc.purpose, got, tc.want)
		}
	}

	// An explicit answer beats the purpose, in both directions — including the
	// false that a plain bool could not have expressed.
	env := flavourEnv(doblurav1alpha1.FlavorOfficial)
	env.Spec.Purpose = doblurav1alpha1.PurposeProduction
	env.Spec.Update = &doblurav1alpha1.UpdateSpec{OnStart: ptr(true)}
	if !env.Spec.UpdatesOnStart() {
		t.Error("an explicit yes must beat the purpose's no")
	}
	env.Spec.Purpose = doblurav1alpha1.PurposeReview
	env.Spec.Update = &doblurav1alpha1.UpdateSpec{OnStart: ptr(false)}
	if env.Spec.UpdatesOnStart() {
		t.Error("an explicit no must beat the purpose's yes")
	}
}

func hasInit(inits []corev1.Container, name string) bool {
	for _, c := range inits {
		if c.Name == name {
			return true
		}
	}
	return false
}
