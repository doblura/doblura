// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Antoni Romera

package controller

import (
	"os"
	"strings"
	"testing"

	doblurav1alpha1 "github.com/doblura/doblura/api/v1alpha1"
)

// The clone script, printed so it can be run against a real server.
//
// Kept as a test rather than a helper binary because the thing worth protecting
// is that ONE path serves branches, tags and commits: the previous version had a
// fast path and a slow fallback, and the fallback was the one the documentation
// told people to use.
func TestCloneScriptUsesOnePathForEveryRefKind(t *testing.T) {
	for _, ref := range []string{"18.0", "v1.2.3", "d6f73382749b8fc6be1311f6d566f1c48f0cbc34"} {
		s := cloneScript(doblurav1alpha1.AddonRepo{
			Name: "server-tools", URL: "https://github.com/OCA/server-tools.git",
			Ref: ref, Depth: 1,
		})
		if strings.Contains(s, "git clone") {
			t.Errorf("ref %q still uses git clone; the shallow fetch path serves every kind:\n%s", ref, s)
		}
		if !strings.Contains(s, `fetch -q --depth 1 origin "`+ref+`"`) {
			t.Errorf("ref %q is not fetched shallowly:\n%s", ref, s)
		}
		if !strings.Contains(s, "checkout -q FETCH_HEAD") {
			t.Errorf("ref %q is fetched but never checked out:\n%s", ref, s)
		}
	}

	// DOBLURA_PRINT_CLONE=<ref> prints the script, so it can be piped to sh and
	// run against a real server rather than only asserted about.
	if ref := os.Getenv("DOBLURA_PRINT_CLONE"); ref != "" {
		t.Log("\n" + cloneScript(doblurav1alpha1.AddonRepo{
			Name: "server-tools", URL: "https://github.com/OCA/server-tools.git",
			Ref: ref, Depth: 1,
		}))
	}
}

// Credentials must never reach the log or .git/config.
func TestCloneScriptKeepsCredentialsOutOfEverythingThatPersists(t *testing.T) {
	s := cloneScript(doblurav1alpha1.AddonRepo{
		Name: "private", URL: "https://git.example.com/acme/addons.git", Ref: "main", Depth: 1,
	})
	// The userinfo obfuscation has to be on every git command that talks to the
	// network, or one of them leaks the token into the Job's log.
	if strings.Count(s, `sed -E "s#://[^@]*@#://***@#g"`) < 1 {
		t.Errorf("git output is not passed through the userinfo obfuscation:\n%s", s)
	}
	// The credential helper writes nothing to disk; a URL with the token in it
	// would land in .git/config, which outlives the init container.
	if strings.Contains(s, "://$GIT_USER") || strings.Contains(s, "${GIT_PASSWORD}@") {
		t.Errorf("the credential is being put in the URL:\n%s", s)
	}
}
