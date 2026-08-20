// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Antoni Romera

package controller

import (
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	doblurav1alpha1 "github.com/doblura/doblura/api/v1alpha1"
)

func terminated(name, msg string) corev1.ContainerStatus {
	return corev1.ContainerStatus{
		Name:  name,
		State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{Message: msg}},
	}
}

// The commit each repository resolved to is read back, and it is the whole point.
//
// A build from a branch is not reproducible. The only thing that makes it
// traceable afterwards is a record of the commit it used, so an empty one is the
// question "which code is in this image" left unanswered — while the object says
// Succeeded. The first version of this parser took the last whitespace-separated
// field and discarded anything over 40 characters, which discarded
// "mis-builder=f988ae…" every single time.
func TestTheCommitThatWentIntoTheImageIsRecorded(t *testing.T) {
	sha := "f988ae69e0f62d03d55fa2a5a670c109fc969047"
	pod := &corev1.Pod{Status: corev1.PodStatus{
		InitContainerStatuses: []corev1.ContainerStatus{
			terminated("clone-mis-builder", "mis-builder="+sha),
			// The fallback puts log lines in the message when a clone fails.
			terminated("clone-noisy", "fatal: could not read from remote\nsomething else"),
			terminated("clone-late", "warming up\nlate="+sha+"\n"),
		},
	}}
	got := commitsIn(pod)
	if got["mis-builder"] != sha {
		t.Errorf("the commit was not read back: %q", got["mis-builder"])
	}
	if got["late"] != sha {
		t.Errorf("a commit after a log line was missed: %q", got["late"])
	}
	if _, ok := got["noisy"]; ok {
		t.Error("a failed clone must not produce a commit")
	}
}

// A build that succeeded with no digest is not a success.
func TestADigestIsOnlyTakenFromTheBuildContainer(t *testing.T) {
	d := "sha256:466292f7c710e213d86dc00b107f3028ed8009b64db7a2eca2a9829acc351c98"
	pod := &corev1.Pod{Status: corev1.PodStatus{
		ContainerStatuses: []corev1.ContainerStatus{terminated("build", d+"\n")},
	}}
	if digestFrom(pod) != d {
		t.Errorf("the digest was not read: %q", digestFrom(pod))
	}
	other := &corev1.Pod{Status: corev1.PodStatus{
		ContainerStatuses: []corev1.ContainerStatus{terminated("sidecar", d)},
	}}
	if digestFrom(other) != "" {
		t.Error("a digest from some other container is not this build's digest")
	}
}

// The tag is derived from the declaration, so the same source lands on the same
// tag and different source cannot silently overwrite it.
func TestTheDerivedTagFollowsTheDeclaration(t *testing.T) {
	b := &doblurav1alpha1.OdooBuild{
		ObjectMeta: metav1.ObjectMeta{Name: "b", Namespace: "n"},
		Spec: doblurav1alpha1.OdooBuildSpec{
			From:  "doblura/odoo:18.0",
			Repos: []doblurav1alpha1.AddonRepo{{Name: "a", URL: "u", Ref: "17.0"}},
		},
	}
	first := tagFor(b)
	if first != tagFor(b) {
		t.Error("the same declaration produced two different tags")
	}
	b.Spec.Repos[0].Ref = "18.0"
	if tagFor(b) == first {
		t.Error("a different ref produced the same tag, so one build overwrites the other")
	}
	b.Spec.To.Tag = "stable"
	if tagFor(b) != "stable" {
		t.Error("a declared tag must be used as written")
	}
}

// No Dockerfile comes from the customer, and the generated one drops .git.
func TestTheGeneratedBuildDropsGitAndLabelsTheAddonsPath(t *testing.T) {
	b := &doblurav1alpha1.OdooBuild{
		ObjectMeta: metav1.ObjectMeta{Name: "b", Namespace: "n"},
		Spec: doblurav1alpha1.OdooBuildSpec{
			From:  "doblura/odoo:18.0",
			Repos: []doblurav1alpha1.AddonRepo{{Name: "mis", URL: "u", Ref: "17.0"}},
			To:    doblurav1alpha1.BuildDestination{Image: "r/x", PushSecret: "s"},
		},
	}
	s := buildScript(b)
	// .git is history, credentials somebody left in a config, and megabytes on
	// every node that pulls the image.
	if !strings.Contains(s, "-name .git -type d -prune") {
		t.Error("the sources go into the image with their .git")
	}
	// ONE addons path, not one per repository: a list of directories somebody has
	// to transcribe into a spec is a list somebody will transcribe wrong.
	if !strings.Contains(s, `LABEL dev.doblura.addons-path="/opt/doblura/addons"`) {
		t.Error("the image does not say where its addons are")
	}
	// And a link farm rather than a flattening, so `ls -l` says which repository
	// each module came from.
	if !strings.Contains(s, "ln -sfn") || !strings.Contains(s, "/opt/doblura/src/$r") {
		t.Error("the modules are not linked into one directory from their sources")
	}
	// TLS on by default: a push over HTTP sends the credential in clear.
	if !strings.Contains(s, "--tls-verify=true") {
		t.Error("the push defaults to something other than TLS")
	}
}
