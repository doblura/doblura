// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Antoni Romera

package controller

import "testing"

// The auxiliary images are configurable, and the setting reaches the pod.
//
// It did not. The chart shipped a ConfigMap offering to pin them, three
// controllers had the names hardcoded, and nothing connected the two — so an
// operator pointing them at an internal registry, which is the only way to install
// this with no route to Docker Hub, got a ConfigMap and pods that went on pulling
// from the internet.
//
// The same shape as the edge middlewares the Ingress referenced and nothing
// created: a setting describing what something else will do, with no something
// else reading it.
func TestTheAuxiliaryImagesCanBePinned(t *testing.T) {
	for _, c := range []struct {
		name, env, fallback string
		get                 func() string
	}{
		{"git", "DOBLURA_GIT_IMAGE", "alpine/git:latest", gitImage},
		{"http", "DOBLURA_HTTP_IMAGE", "curlimages/curl:latest", httpImage},
		{"object store", "DOBLURA_OBJECT_STORE_IMAGE", "rclone/rclone:latest", objectStoreImage},
	} {
		if got := c.get(); got != c.fallback {
			t.Errorf("%s: unset should keep %q, got %q", c.name, c.fallback, got)
		}
		t.Setenv(c.env, "registry.internal/mirror:1.2")
		if got := c.get(); got != "registry.internal/mirror:1.2" {
			t.Errorf("%s: pinning it had no effect, still %q — an air-gapped "+
				"install would go on pulling from the internet", c.name, got)
		}
	}
}
