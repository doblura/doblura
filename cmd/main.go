// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Antoni Romera

// Command manager starts the Doblura operator.
package main

import (
	"context"
	"crypto/tls"
	"errors"
	"flag"
	"net/http"
	"os"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	crwebhook "sigs.k8s.io/controller-runtime/pkg/webhook"

	doblurav1alpha1 "github.com/doblura/doblura/api/v1alpha1"
	"github.com/doblura/doblura/internal/controller"
	doblurawebhook "github.com/doblura/doblura/internal/webhook"
)

var scheme = runtime.NewScheme()

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(doblurav1alpha1.AddToScheme(scheme))
}

func main() {
	var metricsAddr, probeAddr string
	var enableLeaderElection bool
	var webhookOpts doblurawebhook.Options
	var exemptUsers string
	var probeImage string

	flag.StringVar(&metricsAddr, "metrics-bind-address", ":8080", "address the metrics endpoint binds to")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "address the health probes bind to")
	flag.BoolVar(&enableLeaderElection, "leader-elect", false,
		"enable leader election. Mandatory with more than one replica: two "+
			"controllers creating Jobs for the same rehearsal would run it twice.")

	flag.IntVar(&webhookOpts.Port, "webhook-port", 9443,
		"port the admission webhook serves on. 0 disables the webhook, and with it "+
			"the environment quota: only do that if the webhook configurations are "+
			"not installed either, or every create will be refused.")
	flag.StringVar(&webhookOpts.Namespace, "webhook-namespace", os.Getenv("POD_NAMESPACE"),
		"namespace the webhook Service lives in. Part of the certificate's DNS names.")
	flag.StringVar(&webhookOpts.ServiceName, "webhook-service", "",
		"name of the webhook Service. Part of the certificate's DNS names.")
	flag.StringVar(&webhookOpts.CertSecretName, "webhook-cert-secret", "",
		"Secret the self-signed serving certificate is kept in, shared by every replica.")
	flag.StringVar(&webhookOpts.ValidatingConfigName, "validating-webhook-config", "",
		"ValidatingWebhookConfiguration to publish the CA bundle into.")
	flag.StringVar(&webhookOpts.MutatingConfigName, "mutating-webhook-config", "",
		"MutatingWebhookConfiguration to publish the CA bundle into.")
	flag.StringVar(&probeImage, "instance-probe-image", "postgres:18-alpine",
		"image used to probe an OdooInstance: needs psql, sh, df and awk. Its client "+
			"major does not have to match the server's — the probe only reads, it never restores")
	flag.StringVar(&exemptUsers, "quota-exempt-users", "",
		"comma-separated identities the environment quota does not apply to. The "+
			"operator's own ServiceAccount belongs here: it creates environments on "+
			"the cluster's behalf and must not be throttled by a person's allowance.")
	flag.IntVar(&webhookOpts.MaxEnvironmentsPerCreator, "max-environments-per-creator",
		int(doblurawebhook.MaxPerCreatorDefault),
		"how many ephemeral environments one person may hold at once, across every "+
			"customer and namespace. The per-customer limit lives on each OdooTenant.")

	opts := zap.Options{Development: true}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))
	setupLog := ctrl.Log.WithName("setup")

	webhookOpts.ExemptUsers = strings.Split(exemptUsers, ",")
	if err := webhookOpts.Validate(); err != nil {
		setupLog.Error(err, "the webhook configuration is incomplete")
		os.Exit(1)
	}

	restConfig := ctrl.GetConfigOrDie()

	// The serving certificate has to exist BEFORE the manager does: the TLS
	// configuration is an option of the webhook server, not something that can be
	// filled in once it is running. So this is the one piece of work that happens
	// with its own client, before anything is started.
	var webhookServer crwebhook.Server
	var caBundle []byte
	if webhookOpts.Enabled() {
		bootstrap, err := client.New(restConfig, client.Options{Scheme: scheme})
		if err != nil {
			setupLog.Error(err, "unable to build the bootstrap client")
			os.Exit(1)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		bundle, err := doblurawebhook.EnsureServingCert(ctx, bootstrap, webhookOpts.CertOptions())
		cancel()
		if err != nil {
			setupLog.Error(err, "unable to issue the webhook serving certificate")
			os.Exit(1)
		}
		caBundle = bundle.CAPEM
		webhookServer = crwebhook.NewServer(crwebhook.Options{
			Port:    webhookOpts.Port,
			TLSOpts: []func(*tls.Config){bundle.Apply},
		})
	}

	mgr, err := ctrl.NewManager(restConfig, ctrl.Options{
		Scheme:                 scheme,
		Metrics:                metricsserver.Options{BindAddress: metricsAddr},
		HealthProbeBindAddress: probeAddr,
		LeaderElection:         enableLeaderElection,
		LeaderElectionID:       "doblura.dev",
		WebhookServer:          webhookServer,
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

	// The quota. Registered last because it is the only thing here that can refuse
	// somebody's work, and the reader should meet it after the controllers it
	// bounds.
	if webhookOpts.Enabled() {
		if err := doblurawebhook.Register(mgr, webhookOpts, caBundle); err != nil {
			setupLog.Error(err, "unable to register the admission webhooks")
			os.Exit(1)
		}
		// readyz gates on the webhook server actually accepting a TLS connection.
		//
		// Without it the Service starts routing to this pod as soon as the
		// container is up, and a request that arrives before the listener does
		// fails the handshake. With failurePolicy Fail, that is a refused create
		// with a TLS error in it — the least helpful message in the system.
		if err := mgr.AddReadyzCheck("webhook", mgr.GetWebhookServer().StartedChecker()); err != nil {
			setupLog.Error(err, "unable to add the webhook readiness check")
			os.Exit(1)
		}
	}

	if err := (&controller.OdooInstanceReconciler{
		Client:     mgr.GetClient(),
		Scheme:     mgr.GetScheme(),
		ProbeImage: probeImage,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to register controller", "controller", "OdooInstance")
		os.Exit(1)
	}
	// OdooDatabase had no controller at all: the kind existed, its validation
	// existed, its status fields and printer columns existed, and the placement
	// function existed and was tested — and nothing called any of it. A database
	// could be created, pass every rule, and sit there for ever with an empty
	// status.
	if err := (&controller.OdooDatabaseReconciler{
		Client: mgr.GetClient(),
		Scheme: mgr.GetScheme(),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to register controller", "controller", "OdooDatabase")
		os.Exit(1)
	}

	if err := (&controller.OdooBuildReconciler{
		Client: mgr.GetClient(),
		Scheme: mgr.GetScheme(),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to register controller", "controller", "OdooBuild")
		os.Exit(1)
	}

	if err := (&controller.OdooReleaseReconciler{
		Client: mgr.GetClient(),
		Scheme: mgr.GetScheme(),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to register controller", "controller", "OdooRelease")
		os.Exit(1)
	}

	if err := (&controller.OdooRestoreReconciler{
		Client: mgr.GetClient(),
		Scheme: mgr.GetScheme(),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to register controller", "controller", "OdooRestore")
		os.Exit(1)
	}

	if err := (&controller.OdooBackupReconciler{
		Client: mgr.GetClient(),
		Scheme: mgr.GetScheme(),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to register controller", "controller", "OdooBackup")
		os.Exit(1)
	}

	if err := (&controller.OdooReviewSetReconciler{
		Client: mgr.GetClient(),
		Scheme: mgr.GetScheme(),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to register controller", "controller", "ReviewSet")
		os.Exit(1)
	}

	if err := (&controller.OdooTenantReconciler{
		Client: mgr.GetClient(),
		Scheme: mgr.GetScheme(),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to register controller", "controller", "OdooTenant")
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
