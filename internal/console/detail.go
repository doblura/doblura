// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Antoni Romera

package console

import (
	"context"
	"time"

	doblurav1alpha1 "github.com/doblura/doblura/api/v1alpha1"
)

// Two levels of detail, one application.
//
// The temptation is a "simple console" and an "advanced console", and it is the
// wrong shape: two applications drift, and the person who needs the simple one is
// exactly the person who cannot be told "open the other tool" when something is
// wrong. So there is one set of pages and a level, and the level changes how much
// each page says — never which pages exist or what anyone is allowed to do.
//
// The level is DERIVED, not chosen. It was a toggle in the rail, and a toggle is
// a thing somebody has to find, understand and set — on the interface whose whole
// point is that the person fielding a phone call should not have to configure it
// first. It now follows what the API server says the person can actually do:
// somebody who can start a rehearsal or read a Secret is operating the platform
// and wants the object; somebody who can open a throwaway environment and read
// its logs wants to know whether it is up.
//
// Asked rather than tabulated, like every other question this console asks about
// permissions. A list of "technical group names" in Go would be a second copy of
// personas.yaml that goes stale, and would break for anybody who binds the roles
// to their own group names — which is the documented way to install this.
//
// And it is explained rather than merely applied: "Your access" says which level
// you have and what it was derived from, because a screen that quietly shows two
// colleagues different things is a screen they will argue about.
type detailLevel string

const (
	levelPlain    detailLevel = "plain"
	levelAdvanced detailLevel = "advanced"
)

// operatorShaped are the capabilities that mean "you operate the platform".
//
// Starting a rehearsal is the platform's own operation, and reading a Secret is
// the permission no persona but platform has. Either is enough; neither is
// something support or QA can do.
var operatorShaped = []Verb{
	{"create", "odoorehearsals", "", ""},
	{"get", "secrets", "", ""},
}

// levelFor asks the API server what this person is.
//
// A refusal to answer is treated as "not an operator": the simple view is never
// the wrong one to show, and defaulting the other way would put a wall of
// conditions in front of somebody the console could not identify.
func (s *Server) levelFor(ctx context.Context, id Identity) (detailLevel, string) {
	perms, err := s.allowed(ctx, id, operatorShaped...)
	if err != nil {
		return levelPlain, "the API server could not be asked"
	}
	if perms[operatorShaped[0].key()] {
		return levelAdvanced, "you can start a rehearsal"
	}
	if perms[operatorShaped[1].key()] {
		return levelAdvanced, "you can read Secrets"
	}
	return levelPlain, "you open and inspect environments rather than operating the platform"
}

// ── what a non-technical person actually asks ──

// health is the answer to "is it up, and is it slow", in that order.
//
// Neither question is answered by a phase. `Ready` means the operator finished
// building it, which is true of an environment whose pod died a minute ago, and
// says nothing at all about load. So this reads the workload, and every field
// says where its number came from — a status screen that will not show its
// working is one people learn to distrust.
type health struct {
	// State is one of: up, degraded, down, building, asleep, gone.
	State  string
	Detail string

	// Busy is empty when nothing is known. It is deliberately not a percentage
	// of anything invented: it is what the cluster reports, or nothing.
	Busy       string
	BusySource string

	// Since is how long it has been in this state.
	Since string
}

// environmentHealth turns an environment and its workload into an answer.
//
// The `known` flag is the important parameter and it was not here at first. The
// workload read is done as the person, and a persona without access to
// Deployments gets an error the caller was ignoring — so every healthy
// environment was reported as Down, to exactly the people this view exists for.
// "I cannot see" and "it is not running" are different answers and the second
// one starts a false incident.
func environmentHealth(env *doblurav1alpha1.OdooEnvironment, replicas, ready int32, known bool) health {
	h := health{Since: humanSince(env.Status.ReadyAt)}

	switch {
	case env.Status.TerminatedAt != nil:
		h.State, h.Detail = "gone", "This environment has been shut down."
		return h
	case env.Status.Phase == doblurav1alpha1.EnvHibernated:
		h.State = "asleep"
		h.Detail = "Sleeping to save resources. It wakes up when somebody opens it."
		return h
	case env.Status.Phase == doblurav1alpha1.EnvFailed:
		h.State = "down"
		h.Detail = "Something went wrong while preparing it. " + env.Status.Message
		return h
	case env.Status.Phase != doblurav1alpha1.EnvReady:
		h.State = "building"
		h.Detail = "Being prepared. This takes a few minutes, longer if it is restoring a copy."
		return h
	}

	if !known {
		h.State = "unknown"
		h.Detail = "Prepared and finished. Whether it is answering right now cannot be " +
			"read with your access — that needs permission to see the workload."
		return h
	}

	switch {
	case replicas == 0:
		h.State, h.Detail = "down", "It is ready, but nothing is running it."
	case ready == 0:
		h.State = "down"
		h.Detail = "Running but not answering. Usually the database, or it is still starting."
	case ready < replicas:
		h.State = "degraded"
		h.Detail = "Answering, but not every copy is healthy. People may see it slow down."
	default:
		h.State, h.Detail = "up", "Answering normally."
	}
	return h
}

// hibernationHint explains the one state that generates the most confused tickets.
func hibernationHint(env *doblurav1alpha1.OdooEnvironment) string {
	// The TYPE, not the presence of a ttl. spec.lifecycle.ttl carries a schema
	// default, so it is never nil — testing it told every persistent staging
	// environment that it would delete itself in three days, in the calmest
	// possible language, on the page a non-technical person reads. Third time
	// this default has caught something in this project.
	if env.Spec.Lifecycle.Type != doblurav1alpha1.LifecycleEphemeral {
		return ""
	}
	if env.Spec.Lifecycle.TTL == nil {
		return ""
	}
	if env.Status.ReadyAt == nil {
		return ""
	}
	d := env.Spec.Lifecycle.TTL.Duration
	left := time.Until(env.Status.ReadyAt.Time.Add(d))
	if left <= 0 {
		return "Its time is up; it will be removed shortly."
	}
	return "It removes itself in " + shortDuration(left) + "."
}

func shortDuration(d time.Duration) string {
	switch {
	case d < time.Hour:
		return plural(int(d.Minutes()), "minute")
	case d < 48*time.Hour:
		return plural(int(d.Hours()), "hour")
	default:
		return plural(int(d.Hours()/24), "day")
	}
}

// plural, because "in 1 hours" is the kind of detail that makes a person trust
// the rest of the page a little less.
func plural(n int, unit string) string {
	if n == 1 {
		return "1 " + unit
	}
	return itoa(n) + " " + unit + "s"
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	neg := i < 0
	if neg {
		i = -i
	}
	var b [20]byte
	p := len(b)
	for i > 0 {
		p--
		b[p] = byte('0' + i%10)
		i /= 10
	}
	if neg {
		p--
		b[p] = '-'
	}
	return string(b[p:])
}
