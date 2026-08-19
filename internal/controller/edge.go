// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Antoni Romera

package controller

import (
	"context"
	"fmt"
	"strings"

	"golang.org/x/crypto/bcrypt"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	doblurav1alpha1 "github.com/doblura/doblura/api/v1alpha1"
)

// The edge: what stands between the internet and an Odoo.
//
// This file exists because the Ingress referenced middlewares that nothing
// created. The annotation said
//
//	traefik.ingress.kubernetes.io/router.middlewares:
//	  demo-x-noindex@kubernetescrd,demo-x-ratelimit@kubernetescrd
//
// and there was no such object anywhere in the cluster; Traefik said so on every
// reconcile — `middleware "demo-x-ratelimit@kubernetescrd" does not exist` — and
// nothing in doblura was listening. So spec.exposure.auth, noIndex and
// rateLimitRPS were all declared, validated at admission with rules whose comment
// says they are "not advice", and then applied by nobody. A public environment
// could be answering the internet with no authentication in front of it.
//
// The lesson is not "add the missing objects". It is that a field which describes
// what SOMETHING ELSE will do needs the something else to be created by the same
// controller and checked in the same test, or it is a promise with no mechanism.
//
// Traefik here, because Traefik is what k3s and k3d ship and what this is
// deployed against. The rules are expressed as intent first — deny everything
// except these networks, do not be indexed, this many requests a second — so a
// second renderer for another controller writes different objects from the same
// answers rather than reimplementing the decision.

// edgeRule is one thing the edge must do, independent of what implements it.
type edgeRule struct {
	// Name is the suffix of the object, and the last part of what the Ingress
	// annotation refers to. It has to match on both sides or the router breaks —
	// which is the failure this file exists to fix, so both come from here.
	Name string
	// Spec is the Traefik Middleware body.
	Spec map[string]any
}

// edgeRules is everything the edge must do for this environment.
//
// The single source of the names. The Ingress builder asks this for what to
// reference, so a rule that is not created is not referenced either — the two
// cannot drift, because they are the same list.
func edgeRules(env *doblurav1alpha1.OdooEnvironment, htpasswdSecret string) []edgeRule {
	var rules []edgeRule

	if env.Spec.IsPublic() {
		switch env.Spec.Exposure.Auth.Type {
		case doblurav1alpha1.IngressAuthBasic:
			rules = append(rules, edgeRule{
				Name: "basicauth",
				Spec: map[string]any{
					"basicAuth": map[string]any{
						"secret": htpasswdSecret,
						// The browser prompt says which environment it is asking
						// about. Somebody with four of these open needs to know
						// which one wants a password.
						"realm": "doblura: " + env.Name,
					},
				},
			})
		case doblurav1alpha1.IngressAuthForward:
			rules = append(rules, edgeRule{
				Name: "forwardauth",
				Spec: map[string]any{
					"forwardAuth": map[string]any{
						"address":             env.Spec.Exposure.Auth.URL(),
						"trustForwardHeader":  true,
						"authResponseHeaders": []any{"X-Forwarded-User"},
					},
				},
			})
		}
	}

	if env.Spec.Exposure.NoIndexes() {
		rules = append(rules, edgeRule{
			Name: "noindex",
			Spec: map[string]any{
				"headers": map[string]any{
					"customResponseHeaders": map[string]any{
						// noarchive as well as noindex: without it a page that
						// was already crawled stays readable from the cache after
						// the header appears.
						"X-Robots-Tag": "noindex, nofollow, noarchive",
					},
				},
			},
		})
	}

	if rps := env.Spec.Exposure.RateLimitRPS; rps != nil && *rps > 0 {
		rules = append(rules, edgeRule{
			Name: "ratelimit",
			Spec: map[string]any{
				"rateLimit": map[string]any{
					"average": int64(*rps),
					// A burst of one second's worth. Odoo loads dozens of assets
					// on the first page: a limit with no burst rejects the first
					// visit of every day and looks like the environment is down.
					"burst": int64(*rps),
				},
			},
		})
	}

	if nets := env.Spec.Exposure.AllowFrom; len(nets) > 0 {
		rules = append(rules, edgeRule{
			Name: "allowfrom",
			Spec: map[string]any{
				"ipAllowList": map[string]any{
					"sourceRange": toAny(nets),
				},
			},
		})
	}

	if env.Spec.Exposure.SendsHSTS() {
		rules = append(rules, edgeRule{
			Name: "hsts",
			Spec: map[string]any{
				"headers": map[string]any{
					"stsSeconds": int64(31536000),
					// Subdomains are NOT included and preload is off, both
					// deliberately. Environments live on generated names under a
					// customer's domain, and an HSTS header that claims the whole
					// domain from one of them makes every other name under it
					// https-only — including ones doblura did not create and
					// cannot fix. This is the setting that is remembered by
					// browsers for a year and cannot be taken back.
					"stsIncludeSubdomains": false,
					"stsPreload":           false,
				},
			},
		})
	}

	return rules
}

// edgeMiddlewareNames is what the Ingress annotation must refer to.
func edgeMiddlewareNames(env *doblurav1alpha1.OdooEnvironment, htpasswdSecret string) []string {
	rules := edgeRules(env, htpasswdSecret)
	out := make([]string, 0, len(rules))
	for _, r := range rules {
		// <namespace>-<name>@kubernetescrd is how Traefik addresses a Middleware
		// from an Ingress annotation.
		out = append(out, env.Namespace+"-"+edgeObjectName(env, r.Name)+"@kubernetescrd")
	}
	return out
}

func edgeObjectName(env *doblurav1alpha1.OdooEnvironment, rule string) string {
	return env.Name + "-" + rule
}

// ensureEdge creates the objects the Ingress refers to.
//
// Called BEFORE the Ingress, so there is no window where the annotation points at
// something that does not exist yet — which is the state this whole file was
// written to end.
func (r *OdooEnvironmentReconciler) ensureEdge(
	ctx context.Context,
	env *doblurav1alpha1.OdooEnvironment,
) (string, error) {
	htpasswd := ""
	if env.Spec.IsPublic() && env.Spec.Exposure.Auth.Type == doblurav1alpha1.IngressAuthBasic {
		var err error
		htpasswd, err = r.ensureHtpasswd(ctx, env)
		if err != nil {
			return "", err
		}
	}

	rules := edgeRules(env, htpasswd)
	wanted := map[string]bool{}
	for _, rule := range rules {
		wanted[edgeObjectName(env, rule.Name)] = true
		mw := &unstructured.Unstructured{}
		mw.SetAPIVersion("traefik.io/v1alpha1")
		mw.SetKind("Middleware")
		mw.SetName(edgeObjectName(env, rule.Name))
		mw.SetNamespace(env.Namespace)
		mw.SetLabels(envLabels(env, "edge"))
		if err := unstructured.SetNestedMap(mw.Object, rule.Spec, "spec"); err != nil {
			return "", fmt.Errorf("building the %s rule: %w", rule.Name, err)
		}
		if err := ctrl.SetControllerReference(env, mw, r.Scheme); err != nil {
			return "", err
		}
		if err := r.Patch(ctx, mw, client.Apply, fieldOwner, client.ForceOwnership); err != nil {
			// Not fatal on its own: a cluster with no Traefik CRDs has no
			// Middleware kind, and the environment should still come up with the
			// Ingress it can make. Reported, never swallowed.
			return "", fmt.Errorf("applying the %s rule at the edge: %w", rule.Name, err)
		}
	}

	// Rules that were turned off have to be REMOVED, not merely stopped being
	// created: an ipAllowList left behind after somebody deletes allowFrom would
	// go on refusing everybody, from an object nobody remembers.
	var live unstructured.UnstructuredList
	live.SetAPIVersion("traefik.io/v1alpha1")
	live.SetKind("MiddlewareList")
	if err := r.List(ctx, &live, client.InNamespace(env.Namespace),
		client.MatchingLabels{"doblura.dev/environment": env.Name}); err != nil {
		return htpasswd, nil //nolint:nilerr // nothing to clean up if we cannot look
	}
	for i := range live.Items {
		it := &live.Items[i]
		if wanted[it.GetName()] {
			continue
		}
		if err := r.Delete(ctx, it); err != nil && !errors.IsNotFound(err) {
			return "", err
		}
	}

	return htpasswd, nil
}

// ensureHtpasswd is the Secret basic auth reads.
//
// Generated when the environment does not name one, because the alternative is
// worse than it looks: spec.exposure.auth.secretRef was optional, so a public
// environment with BasicAuth and no secret produced a middleware referring to a
// Secret that did not exist — which fails open or closed depending on the
// version, and neither is something to leave to chance in front of a customer's
// data.
//
// One password, kept. Regenerating it on every reconcile would lock out whoever
// had the tab open, which is exactly what the credentials Secret above already
// learnt.
func (r *OdooEnvironmentReconciler) ensureHtpasswd(
	ctx context.Context,
	env *doblurav1alpha1.OdooEnvironment,
) (string, error) {
	if ref := env.Spec.Exposure.Auth.SecretRef; ref != "" {
		return ref, nil
	}

	name := env.Name + "-edge-auth"
	var existing corev1.Secret
	err := r.Get(ctx, client.ObjectKey{Namespace: env.Namespace, Name: name}, &existing)
	if err == nil {
		return name, nil
	}
	if !errors.IsNotFound(err) {
		return "", err
	}

	password, err := randomPassword()
	if err != nil {
		return "", err
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(password), 12)
	if err != nil {
		return "", fmt.Errorf("hashing the edge password: %w", err)
	}

	auth, shown := htpasswdSecrets(env, name, string(hash), password)
	for _, sec := range []*corev1.Secret{auth, shown} {
		if err := ctrl.SetControllerReference(env, sec, r.Scheme); err != nil {
			return "", err
		}
		if err := r.Create(ctx, sec); err != nil && !errors.IsAlreadyExists(err) {
			return "", err
		}
	}
	return name, nil
}

// htpasswdSecrets builds the pair.
//
// TWO Secrets, and this is not tidiness. Traefik's basicAuth refuses a Secret
// with more than one key:
//
//	failed to load basic auth credentials: found 2 elements for secret
//	'demo/x-edge-auth', must be single element exactly
//
// The first version put the hash and the plaintext in one Secret, which is what
// anybody would do — and the middleware then failed to load, the router was
// invalid, and the environment answered 404 on every path, with the reason only
// in the proxy's log.
//
// So the htpasswd Secret holds exactly one key, and the password somebody has to
// be given lives beside it in its own. A hash nobody can reverse, with no record
// anywhere of what it hashes, is a generated address that stops being an
// inconvenience and becomes a lockout.
func htpasswdSecrets(
	env *doblurav1alpha1.OdooEnvironment,
	name, hash, password string,
) (auth, shown *corev1.Secret) {
	auth = &corev1.Secret{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Secret"},
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: env.Namespace,
			Labels: envLabels(env, "edge"),
		},
		Type: corev1.SecretTypeOpaque,
		// One key, called users. Both parts are load-bearing.
		StringData: map[string]string{"users": edgeUser + ":" + hash},
	}
	shown = &corev1.Secret{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Secret"},
		ObjectMeta: metav1.ObjectMeta{
			Name: name + "-password", Namespace: env.Namespace,
			Labels: envLabels(env, "edge"),
		},
		Type: corev1.SecretTypeOpaque,
		StringData: map[string]string{
			"username": edgeUser,
			"password": password,
		},
	}
	return auth, shown
}

const edgeUser = "doblura"

func toAny(in []string) []any {
	out := make([]any, 0, len(in))
	for _, v := range in {
		out = append(out, strings.TrimSpace(v))
	}
	return out
}
