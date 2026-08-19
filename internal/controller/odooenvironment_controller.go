// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Antoni Romera

package controller

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/intstr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	doblurav1alpha1 "github.com/doblura/doblura/api/v1alpha1"
)

// OdooEnvironmentReconciler brings up Odoo environments.
//
// The guarantee that defines this controller
// ──────────────────────────────────────────
// The Ingress is NOT created until the Hardening phase has finished.
//
// The dump OdooSnapshot produces carries the same known password for every user.
// If the Ingress existed during the restore, that Odoo would be serving the
// administrator account to the internet for as long as the process takes. This is
// not an improbable race: restoring a large database takes minutes, and crawlers
// find a new hostname in seconds.
//
// Hence the order: provision → migrate → harden → EXPOSE, with the phases as
// observable state rather than an implementation detail.
type OdooEnvironmentReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=doblura.dev,resources=odooenvironments,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=doblura.dev,resources=odooenvironments/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=services,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=networking.k8s.io,resources=ingresses,verbs=get;list;watch;create;update;patch;delete
// The edge rules the Ingress refers to. Without this the Ingress is created and
// every middleware it names is missing, which is the state edge.go exists to fix.
// +kubebuilder:rbac:groups=traefik.io,resources=middlewares,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=doblura.dev,resources=odoodatabases;odootenants;odooinstances,verbs=get;list;watch
// +kubebuilder:rbac:groups=doblura.dev,resources=odooenvironments/finalizers,verbs=update

const envFinalizer = "doblura.dev/environment-cleanup"

func (r *OdooEnvironmentReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	l := log.FromContext(ctx)

	var env doblurav1alpha1.OdooEnvironment
	if err := r.Get(ctx, req.NamespacedName, &env); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// The finalizer exists only to drop the database, which lives OUTSIDE the
	// cluster and is not reachable by garbage collection. Everything else is a
	// child with an ownerReference and cleans itself up.
	if !env.DeletionTimestamp.IsZero() {
		return r.finalize(ctx, &env)
	}
	if !containsString(env.Finalizers, envFinalizer) {
		env.Finalizers = append(env.Finalizers, envFinalizer)
		return ctrl.Result{}, r.Update(ctx, &env)
	}

	st := env.Status.DeepCopy()
	st.ObservedGeneration = env.Generation

	// TTL. "Ephemeral" with no deadline means "permanent and nobody remembers
	// creating it", and a forgotten environment with real data and a public URL is
	// the incident all of this is trying to prevent.
	if res, done := r.checkExpiry(ctx, &env, st); done {
		return r.commit(ctx, &env, st, res)
	}

	// Mint GitHub App tokens before anything tries to clone. Same reason as the
	// rehearsal: the tokens last an hour, and every phase Job — and every restart
	// of the serving pod — begins with a fresh one.
	if err := mintAppTokensFor(ctx, r.Client, r.Scheme, &env, env.Namespace,
		env.Spec.Addons.Repos); err != nil {
		return ctrl.Result{}, err
	}

	if err := r.ensureConfigAndSecrets(ctx, &env, st); err != nil {
		return ctrl.Result{}, err
	}

	// Closed egress comes BEFORE any pod: created afterwards, there would be a
	// window in which the Odoo can reach the internal network.
	if env.Spec.Security.DenyEgress == nil || *env.Spec.Security.DenyEgress {
		if err := r.ensureEnvNetworkPolicy(ctx, &env); err != nil {
			return ctrl.Result{}, err
		}
	}

	// The phase chain. Each one creates its Job and waits; Owns(&batchv1.Job{})
	// wakes us when it changes, so there is no polling.
	for _, phase := range r.phasePipeline(&env) {
		ready, justCompleted, err := r.runPhase(ctx, &env, st, phase)
		if err != nil {
			return ctrl.Result{}, err
		}
		if !ready {
			return r.commit(ctx, &env, st, ctrl.Result{})
		}
		// Requeue as soon as a step completes. The next step's Job is created on
		// the following pass, and nothing else will trigger it:
		// GenerationChangedPredicate filters our own status writes and the Job
		// that woke us is already final. The rehearsal controller stalled in
		// Asserting for exactly this reason during the first real run.
		if justCompleted {
			return r.commit(ctx, &env, st, ctrl.Result{Requeue: true})
		}
	}

	// From here the environment is hardened: passwords randomised, external
	// credentials stripped. Only now may it be served.
	if err := r.ensureWorkload(ctx, &env, st); err != nil {
		return ctrl.Result{}, err
	}
	if err := r.ensureService(ctx, &env); err != nil {
		return ctrl.Result{}, err
	}

	// The multi-tenancy guardrail, applied at the last possible moment: right
	// before the environment becomes reachable.
	//
	// Late on purpose. Refusing at creation would mean refusing before the
	// catalogue is populated, and the failure would be indistinguishable from a
	// typo. Here the environment exists, is hardened, and is simply not published
	// — with a condition saying exactly who else is in that database. The data is
	// still in the cluster, so this is a publication gate, not data protection;
	// what it prevents is handing a customer their competitor's business data.
	decision, err := CheckHandover(ctx, r.Client, env.Namespace,
		env.Spec.SourceDatabase, env.Spec.ForTenant)
	if err != nil {
		return ctrl.Result{}, err
	}
	meta.SetStatusCondition(&st.Conditions, metav1.Condition{
		Type:               "HandoverAllowed",
		Status:             boolCondition(decision.Allowed),
		Reason:             map[bool]string{true: "HandoverPermitted", false: "SharedWithOtherTenants"}[decision.Allowed],
		Message:            decision.Reason,
		ObservedGeneration: env.Generation,
	})

	switch {
	case !decision.Allowed:
		st.Phase = doblurav1alpha1.EnvFailed
		st.Message = "not exposed: " + decision.Reason
		st.URL = ""
		return r.commit(ctx, &env, st, ctrl.Result{})

	case env.Spec.Exposure.Host != "":
		if err := r.ensureIngress(ctx, &env, st); err != nil {
			return ctrl.Result{}, err
		}
	}

	if st.Phase != doblurav1alpha1.EnvHibernated {
		st.Phase = doblurav1alpha1.EnvReady
		// Stamped once and never moved. It is when the environment first became
		// usable, which is not when it was created — restoring a snapshot takes
		// minutes to hours, and the meter must not charge for a copy nobody could
		// open yet. Re-stamping on a wake from hibernation would reset the clock
		// and lose everything consumed before it.
		if st.ReadyAt == nil {
			t := metav1.Now()
			st.ReadyAt = &t
		}
		meta.SetStatusCondition(&st.Conditions, metav1.Condition{
			Type: "Ready", Status: metav1.ConditionTrue, Reason: "EnvironmentServing",
			Message:            "environment hardened and serving",
			ObservedGeneration: env.Generation,
		})
	}

	l.Info("environment ready", "url", st.URL, "phase", st.Phase)
	return r.commit(ctx, &env, st, r.requeueForLifecycle(&env, st))
}

// envPhaseStep is one step of the pipeline that runs before exposing.
type envPhaseStep struct {
	name string
	// condition is the status condition that records this step's completion.
	// Explicit rather than derived from the name: a condition type is part of the
	// observable API and should not change because somebody renamed a variable.
	condition string
	phase     doblurav1alpha1.EnvPhase
	script    func(*doblurav1alpha1.OdooEnvironment) string
}

// phasePipeline builds the chain from what was declared.
//
// Hardening is ALWAYS there, even for an environment with demo data: it is cheap,
// and it stops a future change to `data.type` from leaving a silent hole.
func (r *OdooEnvironmentReconciler) phasePipeline(env *doblurav1alpha1.OdooEnvironment) []envPhaseStep {
	var steps []envPhaseStep

	switch env.Spec.Data.Type {
	case doblurav1alpha1.DataSnapshot:
		steps = append(steps, envPhaseStep{"restore", "Restored", doblurav1alpha1.EnvRestoring, envRestoreScript})
	case doblurav1alpha1.DataEmpty, doblurav1alpha1.DataDemo:
		steps = append(steps, envPhaseStep{"init", "Initialised", doblurav1alpha1.EnvProvisioning, envInitScript})
	case doblurav1alpha1.DataLive:
		// Nothing to provision: the database already exists.
	}

	// Migration as a STEP, not a gate. It is the only real difference from
	// OdooRehearsal: there the result of the `-u` decides whether something gets
	// promoted; here it is simply the errand on the way to a usable environment
	// running a future version. Same machinery, different purpose.
	// Only data that came from somewhere ELSE can need migrating. A database this
	// operator just initialised is, by definition, already at the image's version:
	// running `-u` on it does nothing except demand that the image ship
	// click-odoo-contrib, which is how a Demo environment fails with
	// "click-odoo-update: not found" for a step it never needed.
	//
	// The engine field carries a default, so testing it alone made the step
	// unconditional — an optional phase that everybody paid for.
	needsMigration := env.Spec.Data.Type == doblurav1alpha1.DataSnapshot ||
		env.Spec.Data.Type == doblurav1alpha1.DataLive
	if needsMigration && env.Spec.Migration.Engine != "" {
		steps = append(steps, envPhaseStep{"migrate", "Migrated", doblurav1alpha1.EnvProvisioning, envMigrateScript})
	}

	steps = append(steps, envPhaseStep{"harden", "Hardened", doblurav1alpha1.EnvHardening, envHardenScript})
	return steps
}

// runPhase creates a phase's Job and reports whether it has finished.
func (r *OdooEnvironmentReconciler) runPhase(
	ctx context.Context,
	env *doblurav1alpha1.OdooEnvironment,
	st *doblurav1alpha1.OdooEnvironmentStatus,
	step envPhaseStep,
) (ready bool, justCompleted bool, err error) {
	name := env.Name + "-" + step.name
	condType := step.condition

	// Already completed in an earlier reconciliation: never repeated. Re-running a
	// restore against an environment somebody is using would destroy it.
	if c := meta.FindStatusCondition(st.Conditions, condType); c != nil && c.Status == metav1.ConditionTrue {
		return true, false, nil
	}

	st.Phase = step.phase

	job := &batchv1.Job{
		TypeMeta: metav1.TypeMeta{APIVersion: "batch/v1", Kind: "Job"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: env.Namespace,
			Labels:    envLabels(env, step.name),
		},
		Spec: batchv1.JobSpec{
			BackoffLimit: ptr(int32(0)),
			Template:     envJobPod(env, step),
		},
	}
	if err := ctrl.SetControllerReference(env, job, r.Scheme); err != nil {
		return false, false, err
	}
	if err := r.Patch(ctx, job, client.Apply, fieldOwner, client.ForceOwnership); err != nil {
		return false, false, fmt.Errorf("applying the Job for phase %s: %w", step.name, err)
	}

	var live batchv1.Job
	if err := r.Get(ctx, client.ObjectKeyFromObject(job), &live); err != nil {
		return false, false, err
	}

	// Read what the clone containers resolved to, whatever the phase did. Done
	// before the switch because a FAILED phase is exactly when somebody wants to
	// know which commit it was running.
	r.recordAddonRevisions(ctx, env, st, name)

	switch {
	case live.Status.Succeeded > 0:
		meta.SetStatusCondition(&st.Conditions, metav1.Condition{
			Type: condType, Status: metav1.ConditionTrue,
			Reason: "PhaseCompleted", Message: "phase " + step.name + " completed",
			ObservedGeneration: env.Generation,
		})
		return true, true, nil
	case live.Status.Failed > 0:
		st.Phase = doblurav1alpha1.EnvFailed
		st.Message = "phase " + step.name + " failed; check the logs of Job " + name
		meta.SetStatusCondition(&st.Conditions, metav1.Condition{
			Type: condType, Status: metav1.ConditionFalse,
			Reason: "PhaseFailed", Message: st.Message,
			ObservedGeneration: env.Generation,
		})
		return false, false, nil
	}
	return false, false, nil
}

// ensureConfigAndSecrets generates the odoo.conf and the environment credentials.
func (r *OdooEnvironmentReconciler) ensureConfigAndSecrets(
	ctx context.Context,
	env *doblurav1alpha1.OdooEnvironment,
	st *doblurav1alpha1.OdooEnvironmentStatus,
) error {
	cm := &corev1.ConfigMap{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "ConfigMap"},
		ObjectMeta: metav1.ObjectMeta{
			Name: env.Name + "-odoo-conf", Namespace: env.Namespace,
			Labels: envLabels(env, "config"),
		},
		Data: map[string]string{
			"odoo.conf": envOdooConf(env),
			// Written unconditionally, even with no cron tier: it costs a few
			// hundred bytes, and generating it only when the tier exists would
			// make enabling the tier a two-step dance where the Deployment can
			// start before the ConfigMap that configures it has been updated.
			"odoo-cron.conf": envCronConf(env),
		},
	}
	if err := ctrl.SetControllerReference(env, cm, r.Scheme); err != nil {
		return err
	}
	if err := r.Patch(ctx, cm, client.Apply, fieldOwner, client.ForceOwnership); err != nil {
		return err
	}

	if env.Spec.Security.RandomizeUserPasswords != nil && !*env.Spec.Security.RandomizeUserPasswords {
		return nil
	}

	// The adminUsers' passwords are generated ONCE and kept: regenerating them on
	// every reconciliation would lock out whoever had a session open, halfway
	// through their testing.
	secName := env.Name + "-credentials"
	var existing corev1.Secret
	err := r.Get(ctx, client.ObjectKey{Namespace: env.Namespace, Name: secName}, &existing)
	if err == nil {
		st.CredentialsSecret = secName
		return nil
	}
	if !errors.IsNotFound(err) {
		return err
	}

	admins := env.Spec.Security.AdminUsers
	if len(admins) == 0 {
		admins = []string{"admin"}
	}
	data := map[string]string{}
	for _, u := range admins {
		pw, err := randomPassword()
		if err != nil {
			return err
		}
		data[u] = pw
	}

	sec := &corev1.Secret{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Secret"},
		ObjectMeta: metav1.ObjectMeta{
			Name: secName, Namespace: env.Namespace,
			Labels: envLabels(env, "credentials"),
		},
		Type:       corev1.SecretTypeOpaque,
		StringData: data,
	}
	if err := ctrl.SetControllerReference(env, sec, r.Scheme); err != nil {
		return err
	}
	if err := r.Create(ctx, sec); err != nil && !errors.IsAlreadyExists(err) {
		return err
	}
	st.CredentialsSecret = secName
	return nil
}

// ensureWorkload creates the Deployment. Zero replicas when hibernated.
func (r *OdooEnvironmentReconciler) ensureWorkload(
	ctx context.Context,
	env *doblurav1alpha1.OdooEnvironment,
	st *doblurav1alpha1.OdooEnvironmentStatus,
) error {
	replicas := int32(1)
	if st.Phase == doblurav1alpha1.EnvHibernated {
		replicas = 0
	}

	dep := &appsv1.Deployment{
		TypeMeta: metav1.TypeMeta{APIVersion: "apps/v1", Kind: "Deployment"},
		ObjectMeta: metav1.ObjectMeta{
			Name: env.Name, Namespace: env.Namespace, Labels: envLabels(env, "odoo"),
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			// Recreate rather than RollingUpdate: two Odoo versions against the
			// same schema at once is asking for trouble, and the filestore is RWO.
			Strategy: appsv1.DeploymentStrategy{Type: appsv1.RecreateDeploymentStrategyType},
			Selector: &metav1.LabelSelector{MatchLabels: envSelector(env)},
			Template: envServingPod(env),
		},
	}
	if err := ctrl.SetControllerReference(env, dep, r.Scheme); err != nil {
		return err
	}
	if err := r.Patch(ctx, dep, client.Apply, fieldOwner, client.ForceOwnership); err != nil {
		return err
	}
	return r.ensureCronTier(ctx, env, replicas)
}

// ensureCronTier creates the cron Deployment, or removes it.
//
// The removal half is the half that matters. The web tier's odoo.conf carries
// max_cron_threads = 0 for exactly as long as a cron tier exists to pick them up,
// so creating the tier and deleting the tier are not "add a Deployment" and
// "stop adding a Deployment" — they are two halves of a switch, and reconciling
// only forwards would leave an environment whose web tier has stopped running
// crons and whose cron tier no longer exists. Nothing would run them at all, and
// nothing would say so: no error, no event, no failing pod. Just an Odoo where
// scheduled actions quietly never fire.
//
// That is why this deletes rather than scales to zero. A Deployment sitting at
// zero replicas reads, to anyone running kubectl get, exactly like a cron tier
// that is temporarily down.
func (r *OdooEnvironmentReconciler) ensureCronTier(
	ctx context.Context,
	env *doblurav1alpha1.OdooEnvironment,
	replicas int32,
) error {
	name := env.Name + "-cron"

	if !env.Spec.Workload.SplitsCrons() {
		dep := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: env.Namespace}}
		return client.IgnoreNotFound(r.Delete(ctx, dep))
	}

	// Hibernation stops the crons with everything else. A hibernated environment
	// that kept firing scheduled actions would be running the very jobs — mail,
	// invoicing, stock moves — that hibernation exists to stop.
	dep := &appsv1.Deployment{
		TypeMeta: metav1.TypeMeta{APIVersion: "apps/v1", Kind: "Deployment"},
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: env.Namespace, Labels: envTierLabels(env, "cron"),
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			// Recreate, and here it is not a preference. Two cron workers against
			// one database is the double-execution this whole tier exists to
			// prevent, and a RollingUpdate deliberately runs both for a while.
			Strategy: appsv1.DeploymentStrategy{Type: appsv1.RecreateDeploymentStrategyType},
			Selector: &metav1.LabelSelector{MatchLabels: envTierSelector(env, "cron")},
			Template: envCronPod(env),
		},
	}
	if err := ctrl.SetControllerReference(env, dep, r.Scheme); err != nil {
		return err
	}
	return r.Patch(ctx, dep, client.Apply, fieldOwner, client.ForceOwnership)
}

func (r *OdooEnvironmentReconciler) ensureService(ctx context.Context, env *doblurav1alpha1.OdooEnvironment) error {
	svc := &corev1.Service{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Service"},
		ObjectMeta: metav1.ObjectMeta{
			Name: env.Name, Namespace: env.Namespace, Labels: envLabels(env, "odoo"),
		},
		Spec: corev1.ServiceSpec{
			Selector: envSelector(env),
			Ports: []corev1.ServicePort{
				{Name: "http", Port: 80, TargetPort: intstrFromString("http")},
				{Name: "websocket", Port: 8072, TargetPort: intstrFromString("websocket")},
			},
		},
	}
	if err := ctrl.SetControllerReference(env, svc, r.Scheme); err != nil {
		return err
	}
	return r.Patch(ctx, svc, client.Apply, fieldOwner, client.ForceOwnership)
}

// ensureIngress exposes the environment. Only ever called AFTER hardening.
func (r *OdooEnvironmentReconciler) ensureIngress(
	ctx context.Context,
	env *doblurav1alpha1.OdooEnvironment,
	st *doblurav1alpha1.OdooEnvironmentStatus,
) error {
	// The edge objects FIRST, then the annotation that points at them.
	//
	// The comment that used to be here said the middlewares were "declared
	// elsewhere (the chart)". They were not declared anywhere: no Middleware
	// existed in any namespace, Traefik logged `middleware "..." does not exist`
	// on every reconcile, and spec.exposure's authentication, noindex and rate
	// limit were enforced by nothing. See edge.go.
	htpasswd, err := r.ensureEdge(ctx, env)
	if err != nil {
		return err
	}

	ann := map[string]string{}

	// Provider mode: the load balancer's own controller reads these, in its own
	// vocabulary, and doblura interprets none of them. Copied verbatim and
	// deliberately not validated — a WAF this operator does not run is not a WAF
	// it can make claims about.
	if waf := env.Spec.Exposure.WAF; waf.Inspects() && waf.Mode == doblurav1alpha1.WAFProvider {
		for k, v := range waf.Annotations {
			ann[k] = v
		}
	}

	// One list, used for both. The names cannot drift from the objects because
	// they are generated from the same rules.
	if mw := edgeMiddlewareNames(env, htpasswd); len(mw) > 0 {
		ann["traefik.ingress.kubernetes.io/router.middlewares"] = strings.Join(mw, ",")
	}

	// TLS, and only when something will actually issue it.
	//
	// The Ingress used to declare `secretName: <env>-tls` unconditionally.
	// Nothing created that Secret, so Traefik served its own default certificate
	// and logged `secret demo/x-tls does not exist` for ever: every address
	// worked, every browser warned, and status.url said https:// with no hint the
	// padlock was broken. Now the customer's issuer is what decides — with one,
	// cert-manager is asked and gets the annotation it needs; with none, no
	// certificate is claimed and the status says whose certificate is being
	// served instead.
	tls, issued := r.tlsFor(ctx, env, ann)

	class := "traefik"
	ing := &networkingv1.Ingress{
		TypeMeta: metav1.TypeMeta{APIVersion: "networking.k8s.io/v1", Kind: "Ingress"},
		ObjectMeta: metav1.ObjectMeta{
			Name: env.Name, Namespace: env.Namespace,
			Labels: envLabels(env, "odoo"), Annotations: ann,
		},
		Spec: networkingv1.IngressSpec{
			IngressClassName: &class,
			TLS:              tls,
			Rules: []networkingv1.IngressRule{{
				Host: env.Spec.Exposure.Host,
				IngressRuleValue: networkingv1.IngressRuleValue{
					HTTP: &networkingv1.HTTPIngressRuleValue{
						Paths: ingressPaths(env),
					},
				},
			}},
		},
	}
	if err := ctrl.SetControllerReference(env, ing, r.Scheme); err != nil {
		return err
	}
	if err := r.Patch(ctx, ing, client.Apply, fieldOwner, client.ForceOwnership); err != nil {
		return err
	}
	st.URL = "https://" + env.Spec.Exposure.Host
	// Said out loud when nobody is issuing a certificate for this address. A
	// status that reads https:// while the ingress controller serves its own
	// self-signed default is a status that teaches people to click through
	// certificate warnings, which is the habit that makes the warning useless.
	st.TLS = doblurav1alpha1.TLSIssued
	if !issued {
		st.TLS = doblurav1alpha1.TLSDefaultCertificate
	}
	st.WAF = doblurav1alpha1.WAFNone
	if w := env.Spec.Exposure.WAF; w.Inspects() {
		st.WAF = w.Mode
	}
	return nil
}

// tlsFor decides whether to claim a certificate, and asks for one if so.
func (r *OdooEnvironmentReconciler) tlsFor(
	ctx context.Context,
	env *doblurav1alpha1.OdooEnvironment,
	ann map[string]string,
) ([]networkingv1.IngressTLS, bool) {
	secret := env.Name + "-tls"
	claim := []networkingv1.IngressTLS{{
		Hosts:      []string{env.Spec.Exposure.Host},
		SecretName: secret,
	}}

	// An issuer on the customer record: cert-manager is asked for it. The
	// annotation is what makes cert-manager act on an Ingress at all; without it
	// the TLS block is a reference to a Secret nobody will ever create, which is
	// the state this replaced.
	var tenant doblurav1alpha1.OdooTenant
	if env.Spec.ForTenant != "" {
		if err := r.Get(ctx, client.ObjectKey{
			Namespace: env.Namespace, Name: env.Spec.ForTenant,
		}, &tenant); err == nil && tenant.Spec.CertIssuer != "" {
			kind, name := tenant.Spec.IssuerKindAndName()
			switch kind {
			case "ClusterIssuer":
				ann["cert-manager.io/cluster-issuer"] = name
			default:
				ann["cert-manager.io/issuer"] = name
			}
			return claim, true
		}
	}

	// No issuer. If somebody has put the Secret there by hand — a certificate
	// bought and loaded, which is a perfectly ordinary way to run this — it is
	// used. Otherwise nothing is claimed.
	var existing corev1.Secret
	if err := r.Get(ctx, client.ObjectKey{
		Namespace: env.Namespace, Name: secret,
	}, &existing); err == nil {
		return claim, true
	}

	return nil, false
}

// ensureEnvNetworkPolicy fences in the environment's Odoo.
func (r *OdooEnvironmentReconciler) ensureEnvNetworkPolicy(ctx context.Context, env *doblurav1alpha1.OdooEnvironment) error {
	tcp := corev1.ProtocolTCP
	udp := corev1.ProtocolUDP
	// The CONFIGURED port, not 5432. This was hardcoded, so an environment
	// pointed at a Postgres on any other port was silently strangled by its own
	// egress policy: pgbouncer reported "connect failed", the pod reported
	// "server down", and nothing anywhere named the policy that was dropping the
	// packets. Note it is Port and not ConnectPort — the pod's outbound
	// connection is the one the PROXY makes, and the proxy dials the real server.
	pg := intstrFromInt(orDefaultInt32(env.Spec.Database.Port, 5432))
	dns := intstrFromInt(53)

	np := &networkingv1.NetworkPolicy{
		TypeMeta: metav1.TypeMeta{APIVersion: "networking.k8s.io/v1", Kind: "NetworkPolicy"},
		ObjectMeta: metav1.ObjectMeta{
			Name: env.Name + "-deny-egress", Namespace: env.Namespace,
			Labels: envLabels(env, "odoo"),
		},
		Spec: networkingv1.NetworkPolicySpec{
			// Selected by environment, not by envSelector: envSelector is the
			// WEB tier's identity, and a policy scoped to it would leave the
			// cron tier — same database credential, same network — with
			// unrestricted egress. The environment label is on every pod.
			PodSelector: metav1.LabelSelector{
				MatchLabels: map[string]string{"doblura.dev/environment": env.Name},
			},
			// Egress only: ingress is Traefik's job, and blocking it here would
			// make the environment unreachable.
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeEgress},
			Egress:      egressRules(env, &tcp, &udp, &dns, &pg),
		},
	}
	if err := ctrl.SetControllerReference(env, np, r.Scheme); err != nil {
		return err
	}
	return r.Patch(ctx, np, client.Apply, fieldOwner, client.ForceOwnership)
}

// checkExpiry applies the TTL.
func (r *OdooEnvironmentReconciler) checkExpiry(
	ctx context.Context,
	env *doblurav1alpha1.OdooEnvironment,
	st *doblurav1alpha1.OdooEnvironmentStatus,
) (ctrl.Result, bool) {
	if env.Spec.Lifecycle.Type != doblurav1alpha1.LifecycleEphemeral {
		return ctrl.Result{}, false
	}
	ttl := 72 * time.Hour
	if env.Spec.Lifecycle.TTL != nil {
		ttl = env.Spec.Lifecycle.TTL.Duration
	}
	expiry := env.CreationTimestamp.Add(ttl)
	if st.ExpiresAt == nil {
		t := metav1.NewTime(expiry)
		st.ExpiresAt = &t
	}

	if time.Now().Before(expiry) {
		return ctrl.Result{}, false
	}

	// Expired: mark and delete. Deleting the object fires the finalizer, which
	// drops the database, and garbage collection takes the rest.
	st.Phase = doblurav1alpha1.EnvExpired
	st.Message = fmt.Sprintf("expired after %s; being destroyed", ttl)
	// Recorded BEFORE the delete, and this ordering is the point: once the object
	// is gone nothing can be asked when it stopped, so the accounting watermark
	// would keep accruing against an environment that no longer exists. Writing it
	// here bounds the overcount at one accounting interval.
	if st.TerminatedAt == nil {
		t := metav1.Now()
		st.TerminatedAt = &t
	}
	if _, err := r.commit(ctx, env, st, ctrl.Result{}); err != nil {
		// Could not record when it stopped. Requeue rather than delete: an
		// environment that outlives its TTL by one pass is a smaller problem than
		// one that disappears without ever being accounted for.
		return ctrl.Result{}, true
	}
	_ = r.Delete(ctx, env)
	return ctrl.Result{}, true
}

// requeueForLifecycle schedules the next wake-up.
func (r *OdooEnvironmentReconciler) requeueForLifecycle(
	env *doblurav1alpha1.OdooEnvironment,
	st *doblurav1alpha1.OdooEnvironmentStatus,
) ctrl.Result {
	if env.Spec.Lifecycle.Type == doblurav1alpha1.LifecycleEphemeral && st.ExpiresAt != nil {
		// RequeueAfter is right here: there is a known deadline. Capped at an hour
		// so a two-week TTL does not depend on one enormous timer that a manager
		// restart would lose.
		d := time.Until(st.ExpiresAt.Time)
		if d > time.Hour {
			d = time.Hour
		}
		if d < time.Minute {
			d = time.Minute
		}
		return ctrl.Result{RequeueAfter: d}
	}
	return ctrl.Result{}
}

// dropDeadline bounds how long deletion waits for the database to go.
//
// Long enough for a drop that has to wait on connections to finish, short enough
// that a wedged job does not leave an object nobody can delete.
const dropDeadline = 10 * time.Minute

// finalize drops the database, which garbage collection cannot reach.
//
// This used to launch the job and remove the finalizer in the same pass, on the
// reasoning that an object which cannot be deleted is worse than a database which
// does not get dropped. That reasoning is right and the implementation was still
// wrong, because it left a job running with nothing owning it and nothing
// waiting for it.
//
// What that costs, measured rather than imagined: deleting an environment and
// recreating it under the same name, the new init finished at 10:08:44 and the
// OLD drop job ran at 10:08:45. It dropped the database the successor had just
// created. The recreated environment then failed in its harden phase with
// `database "env_staging_acme" does not exist` — a message pointing at the phase
// that noticed, not at the job from the previous object that caused it.
//
// So deletion now waits for the drop to reach a terminal state, and the escape
// hatch is kept: past the deadline it gives up — but it DELETES the job on the
// way out, so the thing that outlived the object cannot go on to destroy its
// replacement.
func (r *OdooEnvironmentReconciler) finalize(
	ctx context.Context,
	env *doblurav1alpha1.OdooEnvironment,
) (ctrl.Result, error) {
	l := log.FromContext(ctx)

	// An environment with live data is NOT touched: it pointed at an existing
	// database that is not ours. Dropping it would be catastrophic.
	if env.Spec.Data.Type == doblurav1alpha1.DataLive {
		return r.releaseEnv(ctx, env)
	}

	name := env.Name + "-dropdb"
	job := &batchv1.Job{
		TypeMeta: metav1.TypeMeta{APIVersion: "batch/v1", Kind: "Job"},
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: env.Namespace,
			Labels: envLabels(env, "dropdb"),
		},
		Spec: batchv1.JobSpec{
			BackoffLimit: ptr(int32(2)),
			Template:     envJobPod(env, envPhaseStep{"dropdb", "Dropped", doblurav1alpha1.EnvExpired, envDropScript}),
		},
	}
	// No ownerReference: the parent is being deleted, and a child owned by an
	// object under deletion is collected before it can finish.
	if err := r.Patch(ctx, job, client.Apply, fieldOwner, client.ForceOwnership); err != nil && !errors.IsAlreadyExists(err) {
		l.Error(err, "could not launch the database drop; continuing so the object does not become undeletable")
		return r.releaseEnv(ctx, env)
	}

	var cur batchv1.Job
	if err := r.Get(ctx, client.ObjectKey{Name: name, Namespace: env.Namespace}, &cur); err != nil {
		if errors.IsNotFound(err) {
			return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
		}
		return ctrl.Result{}, err
	}

	switch {
	case jobFinished(&cur):
		// Removed rather than left behind: the name is derived from the
		// environment's, so a leftover job is also what a same-name recreate
		// would collide with.
		return r.releaseEnv(ctx, env, name)
	case env.DeletionTimestamp != nil && time.Since(env.DeletionTimestamp.Time) > dropDeadline:
		l.Error(nil, "the database drop did not finish within the deadline; "+
			"deleting the job and releasing the object, so it cannot outlive this "+
			"environment and drop a later one with the same name",
			"job", name, "deadline", dropDeadline)
		return r.releaseEnv(ctx, env, name)
	default:
		return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
	}
}

// jobFinished reports whether a Job will do nothing further.
//
// Read from the conditions rather than from Succeeded/Failed counts: a job with
// retries left has Failed > 0 and is not finished, and treating that as terminal
// would release the object while the drop is still being retried.
func jobFinished(j *batchv1.Job) bool {
	for _, c := range j.Status.Conditions {
		if (c.Type == batchv1.JobComplete || c.Type == batchv1.JobFailed) &&
			c.Status == corev1.ConditionTrue {
			return true
		}
	}
	return false
}

// releaseEnv removes the finalizer, optionally cleaning up jobs first.
func (r *OdooEnvironmentReconciler) releaseEnv(
	ctx context.Context,
	env *doblurav1alpha1.OdooEnvironment,
	jobs ...string,
) (ctrl.Result, error) {
	bg := metav1.DeletePropagationBackground
	for _, n := range jobs {
		j := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: n, Namespace: env.Namespace}}
		if err := r.Delete(ctx, j, &client.DeleteOptions{PropagationPolicy: &bg}); err != nil &&
			!errors.IsNotFound(err) {
			return ctrl.Result{}, err
		}
	}
	env.Finalizers = removeString(env.Finalizers, envFinalizer)
	return ctrl.Result{}, r.Update(ctx, env)
}

func (r *OdooEnvironmentReconciler) commit(
	ctx context.Context,
	env *doblurav1alpha1.OdooEnvironment,
	st *doblurav1alpha1.OdooEnvironmentStatus,
	res ctrl.Result,
) (ctrl.Result, error) {
	if equality.Semantic.DeepEqual(&env.Status, st) {
		return res, nil
	}
	env.Status = *st
	return res, r.Status().Update(ctx, env)
}

// boolCondition converts a decision into a condition status.
func boolCondition(b bool) metav1.ConditionStatus {
	if b {
		return metav1.ConditionTrue
	}
	return metav1.ConditionFalse
}

func (r *OdooEnvironmentReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&doblurav1alpha1.OdooEnvironment{}, builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		Owns(&batchv1.Job{}).
		Owns(&appsv1.Deployment{}).
		// The customer record, because things on it decide what an environment
		// gets. Without this, setting spec.certIssuer on a customer changed
		// nothing at all until somebody happened to edit an environment: the
		// annotation cert-manager needs was never written, every address went on
		// being served with the ingress controller's own certificate, and there
		// was nothing to see. The same is true of the customer's domain and its
		// image catalogue.
		Watches(
			&doblurav1alpha1.OdooTenant{},
			handler.EnqueueRequestsFromMapFunc(r.environmentsOfTenant),
		).
		Named("odooenvironment").
		Complete(r)
}

// environmentsOfTenant is every environment belonging to a customer.
//
// Namespaced to the customer's own namespace: a tenant named acme in one
// namespace has nothing to do with an environment naming acme in another, and
// waking those would be a customer's edit reconciling somebody else's workload.
func (r *OdooEnvironmentReconciler) environmentsOfTenant(
	ctx context.Context,
	obj client.Object,
) []reconcile.Request {
	var envs doblurav1alpha1.OdooEnvironmentList
	if err := r.List(ctx, &envs, client.InNamespace(obj.GetNamespace())); err != nil {
		return nil
	}
	var out []reconcile.Request
	for i := range envs.Items {
		e := &envs.Items[i]
		if e.Spec.ForTenant != obj.GetName() {
			continue
		}
		out = append(out, reconcile.Request{
			NamespacedName: client.ObjectKeyFromObject(e),
		})
	}
	return out
}

// ─────────────────────── helpers ───────────────────────

func randomPassword() (string, error) {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

func containsString(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}

func removeString(ss []string, s string) []string {
	out := ss[:0]
	for _, x := range ss {
		if x != s {
			out = append(out, x)
		}
	}
	return out
}

func svcBackend(name string, port int32) networkingv1.IngressBackend {
	return networkingv1.IngressBackend{
		Service: &networkingv1.IngressServiceBackend{
			Name: name,
			Port: networkingv1.ServiceBackendPort{Number: port},
		},
	}
}

// recordAddonRevisions copies the commit each repo was cloned at into the
// environment's status.
//
// From the init containers' termination messages, which Kubernetes keeps in the
// pod status: the clone container writes `name=sha` and exits. The alternative
// was for the manager to resolve the refs itself with git ls-remote, and that
// would mean the manager holding every customer's repository credential — the
// one thing this design has consistently refused. The pod already has the
// credential because it needs it; the manager only reads back a hash.
//
// Failures here are ignored on purpose. This is a record, not a gate: an
// environment that works but whose revisions could not be read is not a broken
// environment, and turning it into one would be the observability making the
// outage.
func (r *OdooEnvironmentReconciler) recordAddonRevisions(
	ctx context.Context,
	env *doblurav1alpha1.OdooEnvironment,
	st *doblurav1alpha1.OdooEnvironmentStatus,
	jobName string,
) {
	if len(env.Spec.Addons.Repos) == 0 {
		return
	}
	var pods corev1.PodList
	if err := r.List(ctx, &pods, client.InNamespace(env.Namespace),
		client.MatchingLabels{"job-name": jobName}); err != nil {
		return
	}

	// Indexed by name so a repeated phase updates rather than appends, and so the
	// ref each revision belongs to comes from the spec rather than being guessed.
	refOf := make(map[string]string, len(env.Spec.Addons.Repos))
	for _, repo := range env.Spec.Addons.Repos {
		refOf[repo.Name] = repo.Ref
	}
	seen := make(map[string]doblurav1alpha1.AddonRevision, len(refOf))
	for _, existing := range st.AddonRevisions {
		seen[existing.Name] = existing
	}

	now := metav1.Now()
	for i := range pods.Items {
		for _, cs := range pods.Items[i].Status.InitContainerStatuses {
			t := cs.State.Terminated
			if t == nil || t.Message == "" {
				continue
			}
			for _, line := range strings.Split(strings.TrimSpace(t.Message), "\n") {
				name, sha, ok := strings.Cut(strings.TrimSpace(line), "=")
				if !ok || name == "" || sha == "" {
					continue
				}
				if _, declared := refOf[name]; !declared {
					// FallbackToLogsOnError means a failed clone puts log output
					// here instead, and log lines are not name=sha pairs. Only
					// names the spec declared are accepted.
					continue
				}
				if prev, had := seen[name]; had && prev.Revision == sha {
					continue
				}
				seen[name] = doblurav1alpha1.AddonRevision{
					Name: name, Ref: refOf[name], Revision: sha, ObservedAt: &now,
				}
			}
		}
	}

	out := make([]doblurav1alpha1.AddonRevision, 0, len(seen))
	for _, repo := range env.Spec.Addons.Repos {
		if rev, ok := seen[repo.Name]; ok {
			out = append(out, rev)
		}
	}
	st.AddonRevisions = out
}

// egressRules is what the environment may reach.
//
// DNS and its database, always. And HTTPS when — and only when — the environment
// declares git repositories to clone, because otherwise the addons feature and
// the egress policy contradict each other and the policy wins silently: the
// clone container failed to reach github.com, `set -e` did not stop the script
// because the failure was behind a pipe, and the phase reported a confusing git
// error about paths. Nothing anywhere said "a NetworkPolicy dropped this".
//
// This weakens denyEgress for exactly the environments that need it weakened,
// and it is worth being blunt about the trade: an environment that can reach
// github.com over 443 can reach anything else on 443 too. NetworkPolicy has no
// name-based rules, and pinning GitHub's addresses would break the first time
// they change.
//
// The way to keep an environment with no outbound access at all is
// spec.addons.volume — a PVC populated once, out of band — and that is the
// honest recommendation for anything holding a copy of production.
func egressRules(
	env *doblurav1alpha1.OdooEnvironment,
	tcp *corev1.Protocol, udp *corev1.Protocol,
	dns, pg *intstr.IntOrString,
) []networkingv1.NetworkPolicyEgressRule {
	rules := []networkingv1.NetworkPolicyEgressRule{
		{Ports: []networkingv1.NetworkPolicyPort{
			{Protocol: udp, Port: dns}, {Protocol: tcp, Port: dns},
		}},
		{Ports: []networkingv1.NetworkPolicyPort{{Protocol: tcp, Port: pg}}},
	}
	if len(env.Spec.Addons.Repos) > 0 {
		https := intstrFromInt(443)
		ssh := intstrFromInt(22)
		rules = append(rules, networkingv1.NetworkPolicyEgressRule{
			Ports: []networkingv1.NetworkPolicyPort{
				{Protocol: tcp, Port: &https},
				// 22 as well: a deploy key is one of the four supported
				// mechanisms, and it is useless over HTTPS.
				{Protocol: tcp, Port: &ssh},
			},
		})
	}

	// The mail server's port, when mail is configured.
	//
	// The same lesson as the git ports, learned twice: a feature that needs the
	// network and a policy that denies it are not two settings, they are one
	// contradiction, and the policy always wins silently. Odoo reported
	// "Connection refused" to a server that was up and reachable, and nothing
	// anywhere said a NetworkPolicy had dropped the packet.
	//
	// Only the port that was asked for, not a range: a relay on 1025 gets 1025,
	// and an environment with no mail block gets nothing.
	if m := env.Spec.Mail; m != nil {
		port := m.Port
		if port == 0 {
			port = 587
		}
		p := intstrFromInt(port)
		rules = append(rules, networkingv1.NetworkPolicyEgressRule{
			Ports: []networkingv1.NetworkPolicyPort{{Protocol: tcp, Port: &p}},
		})
	}
	return rules
}

// ingressPaths is where each request goes.
//
// A function rather than a literal inside the builder so a test can assert on the
// real routing instead of on a copy of it. The first version of that test compared
// a string constant kept beside the builder, which is a test that passes when the
// two are edited together and says nothing about what Kubernetes receives.
func ingressPaths(env *doblurav1alpha1.OdooEnvironment) []networkingv1.HTTPIngressPath {
	prefix := networkingv1.PathTypePrefix
	return []networkingv1.HTTPIngressPath{
		// BOTH long-poll paths, to the gevent worker on 8072.
		//
		// Odoo renamed it: /longpolling/ up to 15, /websocket from 16. Only
		// /websocket was routed here, so on a 14 or a 15 every long-poll request
		// went to the ordinary HTTP workers on 80 and sat there holding one open —
		// and with workers = 2, two idle chat tabs are the whole environment. The
		// symptom is an Odoo that works until somebody opens Discuss.
		//
		// Both rather than deriving it from the Odoo version, because routing a
		// path a version does not have costs nothing: there is no handler for it,
		// and the gevent worker serves the same application anyway. Looking the
		// version up would add a way to be wrong in exchange for nothing.
		{Path: "/websocket", PathType: &prefix, Backend: svcBackend(env.Name, 8072)},
		{Path: "/longpolling/", PathType: &prefix, Backend: svcBackend(env.Name, 8072)},
		{Path: "/", PathType: &prefix, Backend: svcBackend(env.Name, 80)},
	}
}
