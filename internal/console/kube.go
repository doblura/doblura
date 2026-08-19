// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Antoni Romera

// Package console is the interface the personas share.
//
// The design commitment it implements is in charts/doblura/templates/personas.yaml:
// support, QA and consultancy do not get their own applications, they get their
// own ClusterRoles, and one interface shows each of them the actions their role
// permits on the same list of customers.
//
// That has one consequence which shapes every file here: **the console has no
// permissions of its own.** Every call to the API server is impersonated as the
// logged-in human, so Kubernetes RBAC is the only authorization in the system and
// a bug in this code cannot grant what the person did not already have. There is
// no `if user.IsAdmin()` anywhere in this package, and there must never be one.
//
// It follows that the console cannot decide which buttons to show either. It asks
// the API server, with SelfSubjectAccessReview, as the impersonated user — so the
// screen and the enforcement come from the same source and cannot disagree.
package console

import (
	"context"
	"fmt"
	"sort"
	"strings"

	authzv1 "k8s.io/api/authorization/v1"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"

	doblurav1alpha1 "github.com/doblura/doblura/api/v1alpha1"
)

// Identity is who the request is for. It is never trusted from the request
// itself: it comes from the session, which comes from the identity provider.
type Identity struct {
	User   string
	Groups []string
	// Email and Name are for display only. Authorization uses User and Groups.
	Email string
	Name  string

	// Cluster is WHERE this request is looking, not who it is for.
	//
	// It sits here because the identity is built once per request and every call
	// already carries it, and threading a second parameter through twenty-eight
	// call sites would have been twenty-eight chances to pass the wrong one.
	//
	// It must never appear in an authorization decision, and there is nothing to
	// decide: the console holds only impersonation in each cluster, so what a
	// person may do THERE is answered by that cluster's own RBAC. Picking a
	// cluster changes which API server is asked, never what the answer is allowed
	// to be. Empty means the cluster the console runs in.
	Cluster string
}

// impersonatedConfig is the REST config that acts as the person.
//
// Separated from clientFor because logs and exec are subresources the generic
// client does not model, and they need the config rather than the client — with
// the same impersonation, so the authorisation story does not fork.
func (s *Server) impersonatedConfig(id Identity) (*rest.Config, error) {
	base, err := s.configFor(id.Cluster)
	if err != nil {
		return nil, err
	}
	cfg := rest.CopyConfig(base)
	cfg.Impersonate = rest.ImpersonationConfig{
		UserName: id.User,
		Groups:   id.Groups,
	}
	return cfg, nil
}

// clientFor builds a Kubernetes client that acts AS the person, not as the
// console.
//
// A fresh client per request rather than one shared client: impersonation is a
// property of the REST config, so a shared client would either need the identity
// threaded through every call or would silently act as the last user to log in.
// The cost is a client build per request; the alternative is a cross-user data
// leak that no test would catch.
func (s *Server) clientFor(id Identity) (client.Client, error) {
	cfg, err := s.impersonatedConfig(id)
	if err != nil {
		return nil, err
	}
	return client.New(cfg, client.Options{Scheme: s.scheme})
}

// Verb is an action the interface might offer.
type Verb struct {
	Verb, Resource, Namespace, Name string
}

// Common actions, named once so a typo in a resource name is a compile error in
// one place rather than a button that is silently always hidden.
var (
	CanCreateEnvironment = func(ns string) Verb {
		return Verb{"create", "odooenvironments", ns, ""}
	}
	CanDeleteEnvironment = func(ns, name string) Verb {
		return Verb{"delete", "odooenvironments", ns, name}
	}
	CanApprove = func(ns, name string) Verb {
		return Verb{"patch", "odooenvironments", ns, name}
	}
	CanReadLogs = func(ns string) Verb {
		return Verb{"get", "pods/log", ns, ""}
	}
	CanCreateRehearsal = func(ns string) Verb {
		return Verb{"create", "odoorehearsals", ns, ""}
	}
)

// allowed asks the API server, as the impersonated user, whether each verb is
// permitted.
//
// SelfSubjectAccessReview rather than a table in this codebase. A table would be
// a second copy of the RBAC rules, it would drift from personas.yaml the first
// time somebody edits a Role, and — worse — it would be a copy that lives on the
// side that does not enforce anything. Asking costs one round trip per question
// and cannot be wrong.
func (s *Server) allowed(ctx context.Context, id Identity, verbs ...Verb) (map[string]bool, error) {
	c, err := s.clientFor(id)
	if err != nil {
		return nil, err
	}
	out := make(map[string]bool, len(verbs))
	for _, v := range verbs {
		rev := &authzv1.SelfSubjectAccessReview{
			Spec: authzv1.SelfSubjectAccessReviewSpec{
				ResourceAttributes: &authzv1.ResourceAttributes{
					Group:     doblurav1alpha1.GroupVersion.Group,
					Resource:  v.Resource,
					Namespace: v.Namespace,
					Name:      v.Name,
					Verb:      v.Verb,
				},
			},
		}
		// pods/log is core, not ours. Getting this wrong would ask about a
		// resource that does not exist, which the API server answers with a
		// cheerful "no" and no indication that the question was nonsense.
		if v.Resource == "pods/log" {
			rev.Spec.ResourceAttributes.Group = ""
			rev.Spec.ResourceAttributes.Resource = "pods"
			rev.Spec.ResourceAttributes.Subresource = "log"
		}
		if err := c.Create(ctx, rev); err != nil {
			return nil, fmt.Errorf("access review for %s %s: %w", v.Verb, v.Resource, err)
		}
		out[v.Verb+":"+v.Resource+":"+v.Name] = rev.Status.Allowed
	}
	return out, nil
}

// key is how a template asks about a verb it was given.
func (v Verb) key() string { return v.Verb + ":" + v.Resource + ":" + v.Name }

// configFor is how to reach a cluster.
//
// The console holds a credential per cluster and that credential can do exactly
// one thing: impersonate. It is the same arrangement as locally, deployed once per
// cluster, which is what keeps the property the whole package rests on — the
// console has no permissions of its own, anywhere. A federation built on a
// credential that could read or write would have quietly become one place holding
// access to every customer.
func (s *Server) configFor(name string) (*rest.Config, error) {
	if name == "" || name == s.opt.LocalClusterName {
		return s.cfg, nil
	}
	cfg, ok := s.clusters[name]
	if !ok {
		// Named and unknown. Refused rather than falling back to the local
		// cluster: silently answering about a different cluster than the one on
		// the screen is the worst thing this could do.
		return nil, fmt.Errorf(
			"there is no cluster called %q. This console knows: %s",
			name, strings.Join(s.clusterNames(), ", "))
	}
	return cfg, nil
}

// clusterNames is every cluster this console can reach, local first.
func (s *Server) clusterNames() []string {
	out := []string{s.opt.LocalClusterName}
	for name := range s.clusters {
		out = append(out, name)
	}
	sort.Strings(out[1:])
	return out
}

// Federated reports whether there is more than one cluster to look at.
//
// Everything about the interface stays exactly as it was when there is not, which
// is decision 3: the open project has to remain completely usable on one cluster,
// with no degraded mode and no reminder that a paid thing exists.
func (s *Server) Federated() bool { return len(s.clusters) > 0 }
