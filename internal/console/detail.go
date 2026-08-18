// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Antoni Romera

package console

import (
	"net/http"
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
// The default is the simple level, because the person who has never set it is
// more likely to be the one who needs it.
type detailLevel string

const (
	levelPlain    detailLevel = "plain"
	levelAdvanced detailLevel = "advanced"
	detailCookie              = "doblura_detail"
)

func levelFrom(r *http.Request) detailLevel {
	if v := r.URL.Query().Get("detail"); v == string(levelAdvanced) || v == string(levelPlain) {
		return detailLevel(v)
	}
	if c, err := r.Cookie(detailCookie); err == nil && c.Value == string(levelAdvanced) {
		return levelAdvanced
	}
	return levelPlain
}

// handleDetail records the choice and returns the person to where they were.
//
// A cookie rather than a field on a user object, because there is no user object:
// identities come from an identity provider or a Secret, and inventing a place to
// store a display preference would mean inventing a database. It is a preference,
// not a permission — losing it costs one click.
func (s *Server) handleDetail(w http.ResponseWriter, r *http.Request, _ Identity) {
	level := string(levelPlain)
	if r.URL.Query().Get("to") == string(levelAdvanced) {
		level = string(levelAdvanced)
	}
	http.SetCookie(w, &http.Cookie{
		Name: detailCookie, Value: level, Path: "/", HttpOnly: true,
		Secure: r.TLS != nil, SameSite: http.SameSiteLaxMode, MaxAge: 365 * 24 * 3600,
	})
	back := r.URL.Query().Get("back")
	if back == "" || back[0] != '/' {
		// Only same-site paths. An absolute URL here would make the console an
		// open redirector, which is a phishing primitive on any page that has a
		// login form.
		back = "/"
	}
	http.Redirect(w, r, back, http.StatusSeeOther)
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
