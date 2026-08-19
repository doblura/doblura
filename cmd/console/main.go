// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Antoni Romera

// Command console serves the interface the personas share.
//
// A separate binary from the manager, deliberately. The manager runs with a
// ServiceAccount that can create and delete other people's environments; the
// console runs with one that can do nothing except impersonate. Putting both in
// one process would mean the web-facing half of the system shares an identity
// with the half that has every permission in the cluster, and no amount of
// careful coding inside that process would make that a good idea.
package main

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	metricsv1beta1 "k8s.io/metrics/pkg/apis/metrics/v1beta1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	doblurav1alpha1 "github.com/doblura/doblura/api/v1alpha1"
	"github.com/doblura/doblura/internal/console"
)

func main() {
	var opt console.Options
	var sessionKey string

	flag.StringVar(&opt.Addr, "addr", ":8080", "address to serve on")
	flag.StringVar(&opt.Issuer, "oidc-issuer", "", "OIDC issuer URL")
	flag.StringVar(&opt.ClientID, "oidc-client-id", "", "OIDC client id")
	flag.StringVar(&opt.RedirectURL, "oidc-redirect-url", "", "this console's /auth/callback URL")
	flag.StringVar(&opt.GroupsClaim, "oidc-groups-claim", "groups", "claim carrying group membership")
	flag.StringVar(&opt.LocalAccountsSecret, "local-accounts-secret", "",
		"Secret holding local accounts (user: bcrypt-hash:group,group)")
	flag.StringVar(&opt.LocalClusterName, "cluster-name", "local",
		"what the cluster this console runs in is called on screen")
	flag.StringVar(&opt.ClustersSecret, "clusters-secret", "",
		"Secret holding one kubeconfig per OTHER cluster, one per key. Each must "+
			"authenticate as a ServiceAccount whose only permission there is "+
			"impersonate: the console holds no other access to any cluster")
	flag.StringVar(&opt.Namespace, "namespace", "",
		"namespace of the local accounts Secret; defaults to POD_NAMESPACE")
	flag.StringVar(&opt.DevIdentity, "dev-identity", "",
		"LOCAL ONLY: serve every request as user[:group,group] with no authentication")
	flag.StringVar(&sessionKey, "session-key", "",
		"base64 key signing the session cookie; must be identical across replicas")
	// Before flag.Parse, so `console hash` does not have to satisfy the server's
	// required flags to print a hash.
	if len(os.Args) > 1 && os.Args[1] == "hash" {
		hashPassword(os.Args[2:])
		return
	}
	flag.Parse()

	// Read from the environment rather than a flag: a client secret in a flag is
	// in the pod spec, in `kubectl describe`, and in every process listing.
	opt.ClientSecret = os.Getenv("DOBLURA_OIDC_CLIENT_SECRET")
	if opt.Namespace == "" {
		opt.Namespace = os.Getenv("POD_NAMESPACE")
	}
	if sessionKey == "" {
		sessionKey = os.Getenv("DOBLURA_SESSION_KEY")
	}

	log := zap.New(zap.UseDevMode(opt.DevIdentity != ""))
	ctrl.SetLogger(log)

	switch {
	case sessionKey != "":
		k, err := base64.StdEncoding.DecodeString(sessionKey)
		if err != nil || len(k) < 32 {
			fmt.Fprintln(os.Stderr, "the session key must be at least 32 base64-encoded bytes")
			os.Exit(1)
		}
		opt.SessionKey = k
	case opt.DevIdentity != "":
		// Generated per process, so a restart signs everyone out. Acceptable
		// locally, and refused below in every other case: a console that quietly
		// invents its own key would log people out at random across replicas and
		// look like a session bug rather than a missing configuration.
		opt.SessionKey = make([]byte, 32)
		_, _ = rand.Read(opt.SessionKey)
	default:
		fmt.Fprintln(os.Stderr, "--session-key (or DOBLURA_SESSION_KEY) is required")
		os.Exit(1)
	}

	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if err := doblurav1alpha1.AddToScheme(scheme); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	// metrics.k8s.io, for the "is it slow" reading. Registered unconditionally
	// even though the API may be absent: a scheme entry costs nothing, and
	// without it the List fails with "no kind is registered" — which looks
	// exactly like a missing metrics-server and is not one.
	if err := metricsv1beta1.AddToScheme(scheme); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	cfg, err := ctrl.GetConfig()
	if err != nil {
		fmt.Fprintln(os.Stderr, "no kubeconfig and not running in a cluster:", err)
		os.Exit(1)
	}

	srv, err := console.New(cfg, scheme, opt)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := srv.ListenAndServe(ctx); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
