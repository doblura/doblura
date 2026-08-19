// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Antoni Romera

package controller

import "os"

// The small images doblura runs beside Odoo.
//
// Cloning a repository, fetching a URL and talking to an object store are three
// jobs Odoo's own image cannot do, so doblura runs a container for each. Which
// container was hardcoded — `alpine/git:latest`, `curlimages/curl:latest`,
// `rclone/rclone:latest` — while the chart shipped a ConfigMap offering to
// configure exactly those three.
//
// Nothing read that ConfigMap. So an operator pinning them to an internal
// registry, which is the whole point of the setting and the only way to install
// this somewhere with no route to Docker Hub, got a ConfigMap and no effect: the
// pods went on pulling from the internet and failed with an image error that named
// a registry nobody had configured.
//
// The same shape as the edge middlewares the Ingress referenced and nothing
// created. A setting that describes what something else will do needs the
// something else to read it, or it is a promise with no mechanism.
//
// Read from the environment rather than by reading the ConfigMap here: the chart
// mounts it as env, so the value arrives before the first reconcile and a
// controller that cannot reach the API server still knows which image to use.
// `:latest` stays the default, because that is what it has always been and
// changing behaviour on upgrade is a separate decision from making the setting
// work.
func helperImage(env, fallback string) string {
	if v := os.Getenv(env); v != "" {
		return v
	}
	return fallback
}

// gitImage clones addon repositories.
func gitImage() string { return helperImage("DOBLURA_GIT_IMAGE", "alpine/git:latest") }

// httpImage fetches a snapshot over HTTP.
func httpImage() string { return helperImage("DOBLURA_HTTP_IMAGE", "curlimages/curl:latest") }

// objectStoreImage talks to S3 and friends.
func objectStoreImage() string {
	return helperImage("DOBLURA_OBJECT_STORE_IMAGE", "rclone/rclone:latest")
}
