// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Antoni Romera

package webhook

import (
	"bytes"
	"context"
	"fmt"

	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
)

// ─────────────── Publishing the CA the API server has to trust ───────────────
//
// The chart installs the webhook configurations with NO caBundle, because at
// template time there is no CA to put there. Something has to fill it in, and the
// obvious place — a one-off patch during startup — is wrong for one specific
// reason: Helm has no install order for admissionregistration kinds, so it applies
// them AFTER the Deployment. A manager that patched once at startup would find
// nothing to patch, log it, and leave a fail-closed webhook with an empty caBundle
// behind it. Every OdooEnvironment create would then be refused with a TLS error
// until somebody restarted the pod.
//
// So it is a controller. The configuration appearing IS the event that triggers
// the patch, which also means a caBundle somebody edited by hand, or a
// `helm upgrade` that reverted it, is corrected within a reconcile rather than at
// the next restart.
//
// +kubebuilder:rbac:groups=admissionregistration.k8s.io,resources=validatingwebhookconfigurations;mutatingwebhookconfigurations,verbs=get;list;watch;patch

// CABundleReconciler keeps one webhook configuration's caBundle pointing at the
// CA this manager is serving.
type CABundleReconciler struct {
	client.Client

	// Name is the single configuration object this reconciler owns. Everything
	// else in the cluster is filtered out before Reconcile is called: the
	// operator has no business rewriting anybody else's webhooks, and the
	// predicate is what makes that structural instead of a promise.
	Name string
	// Mutating picks the kind. A reconcile request carries a name and no kind, so
	// the two configurations get one reconciler each rather than one that guesses.
	Mutating bool
	CABundle []byte
}

func (r *CABundleReconciler) newObject() client.Object {
	if r.Mutating {
		return &admissionregistrationv1.MutatingWebhookConfiguration{}
	}
	return &admissionregistrationv1.ValidatingWebhookConfiguration{}
}

func (r *CABundleReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	l := log.FromContext(ctx)

	// The watch already filters by name. This says it again in the handler,
	// because the object is cluster-scoped and rewriting somebody else's webhook
	// configuration is the kind of mistake a lost predicate makes silently.
	if req.Name != r.Name {
		return ctrl.Result{}, nil
	}

	obj := r.newObject()
	if err := r.Get(ctx, req.NamespacedName, obj); err != nil {
		// Not found is not an error: the release may not install the webhook, and
		// the next create of the object wakes us up again.
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	before := obj.DeepCopyObject().(client.Object)

	var stamped int
	switch cfg := obj.(type) {
	case *admissionregistrationv1.MutatingWebhookConfiguration:
		for i := range cfg.Webhooks {
			if !bytes.Equal(cfg.Webhooks[i].ClientConfig.CABundle, r.CABundle) {
				cfg.Webhooks[i].ClientConfig.CABundle = r.CABundle
				stamped++
			}
		}
	case *admissionregistrationv1.ValidatingWebhookConfiguration:
		for i := range cfg.Webhooks {
			if !bytes.Equal(cfg.Webhooks[i].ClientConfig.CABundle, r.CABundle) {
				cfg.Webhooks[i].ClientConfig.CABundle = r.CABundle
				stamped++
			}
		}
	default:
		return ctrl.Result{}, fmt.Errorf("unexpected object type %T", obj)
	}

	if stamped == 0 {
		return ctrl.Result{}, nil
	}
	if err := r.Patch(ctx, obj, client.MergeFrom(before)); err != nil {
		return ctrl.Result{}, fmt.Errorf("publishing the caBundle into %s: %w", r.Name, err)
	}
	l.Info("published the webhook CA bundle", "configuration", r.Name, "webhooks", stamped)
	return ctrl.Result{}, nil
}

func (r *CABundleReconciler) SetupWithManager(mgr ctrl.Manager) error {
	name := r.Name
	mine := predicate.NewPredicateFuncs(func(o client.Object) bool {
		return o.GetName() == name
	})
	kind := "validating"
	if r.Mutating {
		kind = "mutating"
	}
	return ctrl.NewControllerManagedBy(mgr).
		For(r.newObject(), builder.WithPredicates(mine)).
		Named("cabundle-" + kind).
		Complete(r)
}
