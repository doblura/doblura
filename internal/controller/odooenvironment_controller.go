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
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

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
	if env.Spec.Migration.Engine != "" {
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
		Data: map[string]string{"odoo.conf": envOdooConf(env)},
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
	ann := map[string]string{}
	mw := []string{}

	// Traefik middlewares are referenced as <namespace>-<name>@kubernetescrd. They
	// are declared elsewhere (the chart) and only linked here.
	if env.Spec.IsPublic() {
		switch env.Spec.Exposure.Auth.Type {
		case doblurav1alpha1.IngressAuthBasic:
			mw = append(mw, env.Namespace+"-"+env.Name+"-basicauth@kubernetescrd")
		case doblurav1alpha1.IngressAuthForward:
			mw = append(mw, env.Namespace+"-"+env.Name+"-forwardauth@kubernetescrd")
		}
	}
	if env.Spec.Exposure.NoIndex == nil || *env.Spec.Exposure.NoIndex {
		// An environment holding realistic-looking data indexed by Google is a
		// leak that requires nobody to attack anything.
		mw = append(mw, env.Namespace+"-"+env.Name+"-noindex@kubernetescrd")
	}
	if env.Spec.Exposure.RateLimitRPS != nil && *env.Spec.Exposure.RateLimitRPS > 0 {
		mw = append(mw, env.Namespace+"-"+env.Name+"-ratelimit@kubernetescrd")
	}
	if len(mw) > 0 {
		ann["traefik.ingress.kubernetes.io/router.middlewares"] = strings.Join(mw, ",")
	}

	pathType := networkingv1.PathTypePrefix
	class := "traefik"
	ing := &networkingv1.Ingress{
		TypeMeta: metav1.TypeMeta{APIVersion: "networking.k8s.io/v1", Kind: "Ingress"},
		ObjectMeta: metav1.ObjectMeta{
			Name: env.Name, Namespace: env.Namespace,
			Labels: envLabels(env, "odoo"), Annotations: ann,
		},
		Spec: networkingv1.IngressSpec{
			IngressClassName: &class,
			TLS: []networkingv1.IngressTLS{{
				Hosts:      []string{env.Spec.Exposure.Host},
				SecretName: env.Name + "-tls",
			}},
			Rules: []networkingv1.IngressRule{{
				Host: env.Spec.Exposure.Host,
				IngressRuleValue: networkingv1.IngressRuleValue{
					HTTP: &networkingv1.HTTPIngressRuleValue{
						Paths: []networkingv1.HTTPIngressPath{
							{Path: "/websocket", PathType: &pathType, Backend: svcBackend(env.Name, 8072)},
							{Path: "/", PathType: &pathType, Backend: svcBackend(env.Name, 80)},
						},
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
	return nil
}

// ensureEnvNetworkPolicy fences in the environment's Odoo.
func (r *OdooEnvironmentReconciler) ensureEnvNetworkPolicy(ctx context.Context, env *doblurav1alpha1.OdooEnvironment) error {
	tcp := corev1.ProtocolTCP
	udp := corev1.ProtocolUDP
	pg := intstrFromInt(5432)
	dns := intstrFromInt(53)

	np := &networkingv1.NetworkPolicy{
		TypeMeta: metav1.TypeMeta{APIVersion: "networking.k8s.io/v1", Kind: "NetworkPolicy"},
		ObjectMeta: metav1.ObjectMeta{
			Name: env.Name + "-deny-egress", Namespace: env.Namespace,
			Labels: envLabels(env, "odoo"),
		},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{MatchLabels: envSelector(env)},
			// Egress only: ingress is Traefik's job, and blocking it here would
			// make the environment unreachable.
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeEgress},
			Egress: []networkingv1.NetworkPolicyEgressRule{
				{Ports: []networkingv1.NetworkPolicyPort{
					{Protocol: &udp, Port: &dns}, {Protocol: &tcp, Port: &dns},
				}},
				{Ports: []networkingv1.NetworkPolicyPort{{Protocol: &tcp, Port: &pg}}},
			},
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

// finalize drops the database, which garbage collection cannot reach.
func (r *OdooEnvironmentReconciler) finalize(
	ctx context.Context,
	env *doblurav1alpha1.OdooEnvironment,
) (ctrl.Result, error) {
	l := log.FromContext(ctx)

	// An environment with live data is NOT touched: it pointed at an existing
	// database that is not ours. Dropping it would be catastrophic.
	if env.Spec.Data.Type != doblurav1alpha1.DataLive {
		job := &batchv1.Job{
			TypeMeta: metav1.TypeMeta{APIVersion: "batch/v1", Kind: "Job"},
			ObjectMeta: metav1.ObjectMeta{
				Name: env.Name + "-dropdb", Namespace: env.Namespace,
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
		}
	}

	// Remove the finalizer even if dropping the database failed: otherwise the
	// object is undeletable forever and somebody has to edit it by hand.
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
		Named("odooenvironment").
		Complete(r)
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
