// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Antoni Romera

// Command manager starts the Doblura operator.
package main

import (
	"context"
	"errors"
	"flag"
	"net/http"
	"os"
	"time"

	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	doblurav1alpha1 "github.com/doblura/doblura/api/v1alpha1"
	"github.com/doblura/doblura/internal/controller"
)

var scheme = runtime.NewScheme()

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(doblurav1alpha1.AddToScheme(scheme))
}

func main() {
	var metricsAddr, probeAddr string
	var enableLeaderElection bool

	flag.StringVar(&metricsAddr, "metrics-bind-address", ":8080", "address the metrics endpoint binds to")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "address the health probes bind to")
	flag.BoolVar(&enableLeaderElection, "leader-elect", false,
		"enable leader election. Mandatory with more than one replica: two "+
			"controllers creating Jobs for the same rehearsal would run it twice.")
	opts := zap.Options{Development: true}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))
	setupLog := ctrl.Log.WithName("setup")

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:                 scheme,
		Metrics:                metricsserver.Options{BindAddress: metricsAddr},
		HealthProbeBindAddress: probeAddr,
		LeaderElection:         enableLeaderElection,
		LeaderElectionID:       "doblura.dev",
	})
	if err != nil {
		setupLog.Error(err, "unable to create the manager")
		os.Exit(1)
	}

	if err := (&controller.OdooRehearsalReconciler{
		Client: mgr.GetClient(),
		Scheme: mgr.GetScheme(),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to register controller", "controller", "OdooRehearsal")
		os.Exit(1)
	}

	if err := (&controller.OdooSnapshotReconciler{
		Client: mgr.GetClient(),
		Scheme: mgr.GetScheme(),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to register controller", "controller", "OdooSnapshot")
		os.Exit(1)
	}

	if err := (&controller.OdooEnvironmentReconciler{
		Client: mgr.GetClient(),
		Scheme: mgr.GetScheme(),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to register controller", "controller", "OdooEnvironment")
		os.Exit(1)
	}

	// The only controller that calls out of the cluster. If the manager's
	// namespace has a restrictive egress policy this is the one that stops
	// working, and the only symptom is "cannot reach runboat" in a status.
	if err := (&controller.RunboatLinkReconciler{
		Client: mgr.GetClient(),
		Scheme: mgr.GetScheme(),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to register controller", "controller", "RunboatLink")
		os.Exit(1)
	}

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to add healthz check")
		os.Exit(1)
	}
	// readyz gates on the caches being synced, not on a bare ping.
	//
	// The scaffolded default is healthz.Ping for both, and it lies: the manager
	// answers 200 while its informers are failing to sync, so the Deployment
	// reports Ready, `helm --wait` returns happily and `kubectl rollout status`
	// says it is fine — all while the operator crash-loops and reconciles
	// nothing. That is exactly what happened on the first real deployment.
	if err := mgr.AddReadyzCheck("readyz", func(_ *http.Request) error {
		// Short deadline: this is a probe, not a wait. If the caches are not
		// synced yet, report not-ready and let Kubernetes ask again.
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if !mgr.GetCache().WaitForCacheSync(ctx) {
			return errors.New("informer caches are not synced yet")
		}
		return nil
	}); err != nil {
		setupLog.Error(err, "unable to add readyz check")
		os.Exit(1)
	}

	setupLog.Info("starting the manager")
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		setupLog.Error(err, "the manager exited with an error")
		os.Exit(1)
	}
}
