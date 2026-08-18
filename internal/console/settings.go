// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Antoni Romera

package console

import (
	"net/http"
	"strconv"

	"sigs.k8s.io/controller-runtime/pkg/client"

	doblurav1alpha1 "github.com/doblura/doblura/api/v1alpha1"
)

// Editing an environment.
//
// Everything here already existed in the CRD and could only be reached with
// kubectl, which is the same as not existing for three of the five personas.
//
// The console validates almost nothing, and that is deliberate rather than lazy.
// The rules live in the CRD as CEL and in the admission webhooks — that a cron
// tier needs a filestore both tiers can reach, that a public environment cannot
// be left unauthenticated, that cron replicas are capped at one. Re-checking any
// of that here would be a second copy on the side that enforces nothing, and it
// would be the copy that goes stale. So the form writes what was asked for, the
// API server refuses what it should, and the refusal — in the words of the rule
// that wrote it — goes straight to the screen.
//
// The one thing the console DOES do is read-modify-write rather than patch, so
// two people editing the same environment produce a conflict the API server
// reports, instead of one silently overwriting the other.

func (s *Server) handleEnvironmentSettings(w http.ResponseWriter, r *http.Request, id Identity) {
	ns, name := r.PathValue("ns"), r.PathValue("name")
	if err := r.ParseForm(); err != nil {
		s.fail(w, id, err)
		return
	}
	c, err := s.clientFor(id)
	if err != nil {
		s.fail(w, id, err)
		return
	}

	var env doblurav1alpha1.OdooEnvironment
	if err := c.Get(r.Context(), client.ObjectKey{Namespace: ns, Name: name}, &env); err != nil {
		s.fail(w, id, err)
		return
	}

	applyWorkloadForm(&env, r)
	applyExposureForm(&env, r)

	// Written explicitly whichever way the box went, because the field is a
	// pointer precisely so that "no" survives the purpose's default. Leaving it
	// nil when unticked would hand a review environment straight back to the
	// purpose, which says yes.
	on := r.FormValue("updateOnStart") == "on"
	if env.Spec.Update == nil {
		env.Spec.Update = &doblurav1alpha1.UpdateSpec{}
	}
	env.Spec.Update.OnStart = &on

	// Updated as the person. A persona without patch on odooenvironments gets a
	// 403 naming the verb, the resource and themselves, which is exactly what
	// whoever administers their groups needs.
	if err := c.Update(r.Context(), &env); err != nil {
		s.failBack(w, r, id, err)
		return
	}
	http.Redirect(w, r, "/e/"+ns+"/"+name, http.StatusSeeOther)
}

// applyWorkloadForm sets the web and cron tiers.
func applyWorkloadForm(env *doblurav1alpha1.OdooEnvironment, r *http.Request) {
	if env.Spec.Workload == nil {
		env.Spec.Workload = &doblurav1alpha1.WorkloadSplit{}
	}
	w := env.Spec.Workload

	if v, ok := formInt(r, "webReplicas"); ok {
		if w.Web == nil {
			w.Web = &doblurav1alpha1.WebTier{}
		}
		w.Web.Replicas = v
	}
	if v, ok := formInt(r, "webWorkers"); ok {
		if w.Web == nil {
			w.Web = &doblurav1alpha1.WebTier{}
		}
		w.Web.Workers = &v
	}

	// The cron tier is a switch, and turning it OFF has to remove the tier
	// rather than set it to zero replicas: the operator deletes the Deployment
	// and returns the web tier to running crons itself, and those two halves are
	// one decision. A tier left at zero would mean nobody runs the crons at all.
	if r.FormValue("cronTier") == "on" {
		if w.Cron == nil {
			w.Cron = &doblurav1alpha1.CronTier{Replicas: 1}
		}
		w.Cron.Replicas = 1
		if v, ok := formInt(r, "cronThreads"); ok {
			w.Cron.Threads = v
		}
	} else {
		w.Cron = nil
	}
}

// applyExposureForm sets how the environment is reached.
func applyExposureForm(env *doblurav1alpha1.OdooEnvironment, r *http.Request) {
	e := &env.Spec.Exposure

	public := r.FormValue("public") == "on"
	e.Public = &public
	if h := r.FormValue("host"); h != "" {
		e.Host = h
	}
	if t := r.FormValue("authType"); t != "" {
		e.Auth.Type = doblurav1alpha1.EnvAuthType(t)
	}
	if sr := r.FormValue("authSecret"); sr != "" {
		e.Auth.SecretRef = sr
	}

	noIndex := r.FormValue("noIndex") == "on"
	e.NoIndex = &noIndex

	// An empty rate limit means none, and that has to clear the field rather
	// than leave the previous value: a form that cannot express "off" is a form
	// that traps people in whatever they set once.
	if v, ok := formInt(r, "rateLimitRPS"); ok && v > 0 {
		e.RateLimitRPS = &v
	} else {
		e.RateLimitRPS = nil
	}
}

// formInt reads an integer field, reporting whether it was present and sane.
//
// A blank field is "not set", never zero. Zero is a real answer for several of
// these — zero web replicas is how you stop an environment — so conflating the
// two would make an empty box shut something down.
func formInt(r *http.Request, key string) (int32, bool) {
	raw := r.FormValue(key)
	if raw == "" {
		return 0, false
	}
	n, err := strconv.ParseInt(raw, 10, 32)
	if err != nil {
		return 0, false
	}
	return int32(n), true
}
