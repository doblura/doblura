// SPDX-License-Identifier: Apache-2.0
// Copyright (C) 2026 Antoni Romera

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ─────────────── Runboat, seen from here ───────────────
//
// Runboat already does per-PR Odoo environments, and does them well. The
// non-goal stands: this is not a reimplementation. It is a window.
//
// The problem it solves is not technical, it is that people have two tabs open.
// Support looks at Doblura for the customer's staging and at Runboat for the PR
// that is supposed to fix the ticket, and nothing lines the two up. So Doblura
// mirrors Runboat's builds into its own status and shows them on the same list.
//
// Why a mirror in status, and not one object per build
// ───────────────────────────────────────────────────
//
// The tempting design is to create an OdooEnvironment per Runboat build so
// everything is one type. It is wrong, and the reason is ownership: Doblura did
// not create those builds and cannot reconcile them. Deleting the mirror object
// would not delete the build, an OdooEnvironment's spec (snapshot, hardening,
// exposure) means nothing for one, and the two controllers would fight over
// which one is the truth.
//
// So the mirror is read-only projection, in status, on a single object. Doblura
// is a viewer of Runboat's state — never a second owner of it.
//
// Two things the real API forced
// ──────────────────────────────
//
// Reading Runboat's own router before writing this changed the design twice:
//
//  1. `POST /builds/{name}/start`, `/stop` and `/reset` carry NO authentication
//     in Runboat. Anyone who can reach the API can reset — that is, wipe and
//     reinitialize — any build. That is a reasonable choice for a public CI
//     playground, and it means proxying those actions through Doblura makes them
//     *more* protected than they were, because here the API server authorizes
//     them. It is the strongest argument for the proxy, so it is worth stating.
//
//  2. `DELETE /builds?repo=…` undeploys, in bulk, everything matching a filter,
//     behind a single shared basic-auth credential. Doblura holds that credential
//     to do anything privileged, so one over-broad grant plus one bug could take
//     out every build for a repository. Bulk undeploy is therefore not exposed
//     at all, and even the per-build actions are off until somebody lists them.

// RunboatAction is something a person can ask Doblura to do to a build.
//
// Undeploy is deliberately absent. Runboat's bulk-delete endpoint takes the same
// credential and a filter, and there is no version of "delete every build for
// this repo" that belongs behind a console button.
// +kubebuilder:validation:Enum=Start;Stop;Reset
type RunboatAction string

const (
	// RunboatStart scales a stopped build back up.
	RunboatStart RunboatAction = "Start"

	// RunboatStop scales a build to zero. Reversible.
	RunboatStop RunboatAction = "Stop"

	// RunboatReset redeploys, which reinitializes the database.
	//
	// Destructive: whatever somebody was doing in that build is gone. It is
	// listed because "reset it and try again" is the single most common thing
	// anyone wants from a review environment, but it is the reason
	// actionRequests carry an id.
	RunboatReset RunboatAction = "Reset"
)

// RunboatBuildStatus mirrors Runboat's own BuildStatus verbatim.
//
// Copied rather than mapped onto Doblura's phases on purpose. These are somebody
// else's states, and translating them would invent a correspondence that does
// not exist — `initializing` is not `Restoring`, and pretending otherwise is how
// a mirror starts lying. When the value is unrecognised it is passed through, so
// a Runboat upgrade that adds a state does not blank the column.
type RunboatBuildStatus string

const (
	RunboatStopped      RunboatBuildStatus = "stopped"
	RunboatStopping     RunboatBuildStatus = "stopping"
	RunboatInitializing RunboatBuildStatus = "initializing"
	RunboatStarting     RunboatBuildStatus = "starting"
	RunboatStarted      RunboatBuildStatus = "started"
	RunboatFailed       RunboatBuildStatus = "failed"
	RunboatUndeploying  RunboatBuildStatus = "undeploying"
)

// RunboatAuth is how Doblura authenticates to Runboat.
//
// Runboat uses HTTP basic auth with one shared admin credential, so there is
// exactly one mechanism and no point pretending to offer a choice. It is only
// needed for privileged endpoints: listing builds is public.
type RunboatAuth struct {
	// BasicAuthSecret holds keys "username" and "password".
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	BasicAuthSecret string `json:"basicAuthSecret"`
}

// RunboatFilter narrows what gets mirrored.
//
// Worth setting. A shared Runboat carries every repository the organisation
// tests, and mirroring all of it fills the status with builds nobody on this
// cluster cares about.
type RunboatFilter struct {
	// Repos is a list of "owner/name". Empty mirrors everything.
	//
	// Runboat lowercases repository names, so these are compared case
	// insensitively — otherwise a filter of "OCA/foo" would silently match
	// nothing, which is the least helpful possible failure.
	// +kubebuilder:validation:MaxItems=64
	// +optional
	Repos []string `json:"repos,omitempty"`

	// TargetBranch narrows to one Odoo series, e.g. "17.0".
	// +kubebuilder:validation:MaxLength=128
	// +optional
	TargetBranch string `json:"targetBranch,omitempty"`
}

// RunboatActionRequest asks for one action, once.
//
// The id is what makes this safe. A reconcile can run for any reason at any
// time, and Reset destroys a database — so the controller records each id it has
// executed and never executes the same one twice. Without it, an unrelated spec
// edit would silently re-fire every request in the list.
type RunboatActionRequest struct {
	// ID is an idempotency key. Any unique string; the console generates one per
	// click. Reusing an id is how you say "this is the same request", so
	// changing your mind means a new id, not an edited one.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=64
	ID string `json:"id"`

	// Build is Runboat's build name, as it appears in status.builds.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	Build string `json:"build"`

	// Action is what to do.
	Action RunboatAction `json:"action"`

	// RequestedBy is recorded for the audit trail. The console fills it with the
	// authenticated user; nothing verifies it, so it is a note, not a claim.
	// +kubebuilder:validation:MaxLength=253
	// +optional
	RequestedBy string `json:"requestedBy,omitempty"`
}

// RunboatLinkSpec points at one Runboat and says what may be done through it.
//
// Note the shape of the first rule. `self.allowedActions` on an object that
// simply omitted the field is not an empty list — it is an evaluation ERROR, and
// the API server then rejects every RunboatLink, including valid read-only ones.
// The same trap has now bitten this project three times (OdooEnvironment's
// security block, OdooDatabase's companies), so: guard with has() first, and
// order the clauses so an absent allowedActions with a pending request DENIES
// rather than erroring.
//
// +kubebuilder:validation:XValidation:rule="!has(self.actionRequests) || size(self.actionRequests) == 0 || (has(self.allowedActions) && self.actionRequests.all(r, r.action in self.allowedActions))",message="every actionRequest must name an action listed in spec.allowedActions; allowedActions is the kill switch, and it is checked here so the apply is rejected rather than a Reset being quietly ignored"
// +kubebuilder:validation:XValidation:rule="!has(self.actionRequests) || size(self.actionRequests) == 0 || has(self.auth)",message="requesting actions needs spec.auth: without a credential the controller cannot talk to Runboat's privileged endpoints"
type RunboatLinkSpec struct {
	// URL is Runboat's base URL, without the /api/v1 suffix.
	// +kubebuilder:validation:Pattern=`^https?://[^\s]+$`
	// +kubebuilder:validation:MaxLength=2048
	URL string `json:"url"`

	// Auth is needed only for actions. Listing builds is unauthenticated.
	// +optional
	Auth *RunboatAuth `json:"auth,omitempty"`

	// PollInterval is how often to refresh the mirror.
	//
	// Runboat also offers a server-sent-event stream, which would be cheaper and
	// more immediate. Polling is deliberate for a first version: a long-lived SSE
	// connection that silently dies leaves a mirror that looks current and is
	// not, and a stale mirror is worse than a slow one. The stream is a later
	// optimisation, once the staleness is visible in status.
	// +kubebuilder:default="60s"
	// +optional
	PollInterval *metav1.Duration `json:"pollInterval,omitempty"`

	// +optional
	Filter *RunboatFilter `json:"filter,omitempty"`

	// AllowedActions is what this link permits at all, independent of RBAC.
	//
	// Empty by default, which makes a fresh link a read-only window. Two locks
	// rather than one: RBAC says who may ask, this says what is askable here.
	// Turning off Reset for a Runboat that other teams also use does not require
	// finding and editing everyone's role bindings.
	// +kubebuilder:validation:MaxItems=3
	// +optional
	AllowedActions []RunboatAction `json:"allowedActions,omitempty"`

	// ActionRequests are one-shot requests. The controller executes each new id
	// and records the outcome; removing an entry afterwards is the caller's job
	// and forgetting is harmless.
	// +kubebuilder:validation:MaxItems=32
	// +optional
	ActionRequests []RunboatActionRequest `json:"actionRequests,omitempty"`

	// MaxBuilds caps how many builds are mirrored into status.
	//
	// An etcd object cannot exceed about 1.5 MB, and a busy Runboat has hundreds
	// of builds. Truncation is recorded in status.truncated rather than being
	// silent, because a list that quietly stops at the limit reads exactly like a
	// list of everything.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=500
	// +kubebuilder:default=200
	// +optional
	MaxBuilds int32 `json:"maxBuilds,omitempty"`
}

// RunboatBuild is one mirrored build. Every field comes from Runboat; none is
// computed here.
type RunboatBuild struct {
	// Name is Runboat's build name and the handle for actions.
	Name string `json:"name"`

	// Repo is "owner/name", lowercased by Runboat.
	// +optional
	Repo string `json:"repo,omitempty"`

	// TargetBranch is the Odoo series the PR targets, e.g. "17.0".
	// +optional
	TargetBranch string `json:"targetBranch,omitempty"`

	// PR is the pull request number, absent for branch builds.
	// +optional
	PR *int32 `json:"pr,omitempty"`

	// Commit is the git sha the build was made from.
	// +optional
	Commit string `json:"commit,omitempty"`

	// Status is Runboat's own state, passed through unmapped.
	// +optional
	Status RunboatBuildStatus `json:"status,omitempty"`

	// DeployLink opens the Odoo instance itself.
	// +optional
	DeployLink string `json:"deployLink,omitempty"`

	// WebuiLink opens Runboat's own page for the build, which is where its logs
	// are. Doblura does not proxy those: a link to the thing that owns them beats
	// a copy that can go stale.
	// +optional
	WebuiLink string `json:"webuiLink,omitempty"`

	// PRLink opens the pull request on the forge.
	// +optional
	PRLink string `json:"prLink,omitempty"`

	// Created is when Runboat first deployed it.
	// +optional
	Created *metav1.Time `json:"created,omitempty"`

	// LastScaled is when it was last started or stopped, which is the closest
	// thing Runboat has to "when was this last touched".
	// +optional
	LastScaled *metav1.Time `json:"lastScaled,omitempty"`
}

// RunboatActionResult records what happened to one request id.
type RunboatActionResult struct {
	// ID matches the request.
	ID string `json:"id"`

	// +optional
	Build string `json:"build,omitempty"`

	// +optional
	Action RunboatAction `json:"action,omitempty"`

	// Succeeded is whether Runboat accepted it.
	Succeeded bool `json:"succeeded"`

	// Message carries Runboat's own error when it did not, unaltered. A 404 here
	// usually means the mirror was stale and the build is already gone.
	// +optional
	Message string `json:"message,omitempty"`

	// +optional
	ExecutedAt *metav1.Time `json:"executedAt,omitempty"`

	// RequestedBy is copied from the request, for the audit trail.
	// +optional
	RequestedBy string `json:"requestedBy,omitempty"`
}

// RunboatLinkStatus is written by the controller only.
type RunboatLinkStatus struct {
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// Builds is the mirror, newest first.
	// +optional
	Builds []RunboatBuild `json:"builds,omitempty"`

	// Total is how many builds Runboat reported after filtering, before the
	// MaxBuilds cap. When it exceeds len(builds) the mirror is partial.
	// +optional
	Total int32 `json:"total,omitempty"`

	// Truncated says the cap was hit. Surfaced as a printcolumn so nobody reads
	// a capped list as a complete one.
	// +optional
	Truncated bool `json:"truncated,omitempty"`

	// LastPoll is when the mirror was last refreshed.
	//
	// The most important field here. Everything else in this status is a copy of
	// somebody else's truth, and a copy is only worth what its age says it is.
	// +optional
	LastPoll *metav1.Time `json:"lastPoll,omitempty"`

	// RunboatVersion is nothing Runboat exposes, so it is absent by design —
	// recorded here as a note so nobody adds a field that can only be guessed.

	// ExecutedActions records the ids already carried out, so none is repeated.
	// +kubebuilder:validation:MaxItems=64
	// +optional
	ExecutedActions []RunboatActionResult `json:"executedActions,omitempty"`

	// +optional
	Message string `json:"message,omitempty"`
}

// RunboatLink condition types.
const (
	// ConditionRunboatReachable is whether the last poll succeeded. A link that
	// stops being reachable keeps its builds — going blank would suggest every
	// build disappeared, when what happened is that Doblura stopped being able to
	// see them. The condition and LastPoll say which.
	ConditionRunboatReachable = "Reachable"

	// ConditionMirrorFresh is whether the mirror was refreshed within twice the
	// poll interval. It is what the interface reads to decide whether to show the
	// list normally or with a staleness warning across it.
	ConditionMirrorFresh = "MirrorFresh"
)

// RunboatLink mirrors a Runboat deployment's builds and proxies a small,
// explicitly allowed set of actions on them.
//
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=rbl
// +kubebuilder:printcolumn:name="URL",type=string,JSONPath=`.spec.url`
// +kubebuilder:printcolumn:name="Builds",type=integer,JSONPath=`.status.total`
// +kubebuilder:printcolumn:name="Truncated",type=boolean,JSONPath=`.status.truncated`
// +kubebuilder:printcolumn:name="Reachable",type=string,JSONPath=`.status.conditions[?(@.type=="Reachable")].status`
// +kubebuilder:printcolumn:name="Fresh",type=string,JSONPath=`.status.conditions[?(@.type=="MirrorFresh")].status`
// +kubebuilder:printcolumn:name="Last poll",type=date,JSONPath=`.status.lastPoll`
type RunboatLink struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   RunboatLinkSpec   `json:"spec,omitempty"`
	Status RunboatLinkStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// RunboatLinkList is a list of RunboatLink.
type RunboatLinkList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []RunboatLink `json:"items"`
}

func init() {
	SchemeBuilder.Register(&RunboatLink{}, &RunboatLinkList{})
}

// APIPath returns the base path of Runboat's API for this link.
//
// Runboat mounts its router at /api/v1. Trailing slashes on spec.url are a
// certainty in the field, so they are trimmed rather than producing a URL with a
// double slash that some proxies reject and others silently accept.
func (s *RunboatLinkSpec) APIPath() string {
	u := s.URL
	for len(u) > 0 && u[len(u)-1] == '/' {
		u = u[:len(u)-1]
	}
	return u + "/api/v1"
}

// Allows reports whether this link permits an action.
//
// The default — an empty AllowedActions — denies everything, so a link created
// without thinking about it is a read-only window rather than a remote control.
func (s *RunboatLinkSpec) Allows(a RunboatAction) bool {
	for _, x := range s.AllowedActions {
		if x == a {
			return true
		}
	}
	return false
}

// PollEvery returns the poll interval, defaulted.
//
// A floor of 10s applies. The CRD default is 60s, but nothing stops somebody
// setting 1s, and one RunboatLink should not be able to hammer a shared Runboat
// into the ground.
func (s *RunboatLinkSpec) PollEvery() metav1.Duration {
	const floor = 10
	if s.PollInterval == nil || s.PollInterval.Duration <= 0 {
		return metav1.Duration{Duration: 60e9}
	}
	if s.PollInterval.Duration < floor*1e9 {
		return metav1.Duration{Duration: floor * 1e9}
	}
	return *s.PollInterval
}

// MatchesFilter reports whether a build should be mirrored.
//
// Repo comparison is case insensitive because Runboat lowercases repository
// names on the way in, so a filter written as it appears on GitHub — "OCA/foo" —
// would otherwise match nothing at all and look like a broken link.
func (s *RunboatLinkSpec) MatchesFilter(repo, targetBranch string) bool {
	if s.Filter == nil {
		return true
	}
	if s.Filter.TargetBranch != "" && s.Filter.TargetBranch != targetBranch {
		return false
	}
	if len(s.Filter.Repos) == 0 {
		return true
	}
	for _, r := range s.Filter.Repos {
		if equalFold(r, repo) {
			return true
		}
	}
	return false
}

// equalFold is an ASCII-only case-insensitive compare. Repository names are
// ASCII, and avoiding strings keeps this file free of imports beyond metav1.
func equalFold(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := 0; i < len(a); i++ {
		x, y := a[i], b[i]
		if 'A' <= x && x <= 'Z' {
			x += 'a' - 'A'
		}
		if 'A' <= y && y <= 'Z' {
			y += 'a' - 'A'
		}
		if x != y {
			return false
		}
	}
	return true
}
