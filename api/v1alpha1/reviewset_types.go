// SPDX-License-Identifier: Apache-2.0
// Copyright (C) 2026 Antoni Romera

package v1alpha1

import metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

// ─────────────── Environments that follow a repository ───────────────
//
// Everything else here is created by a person: somebody decides a staging server
// should exist and says so. That is right for staging and wrong for review, where
// the decision was already made when the pull request was opened, and where
// asking a human to mirror it by hand means the environment exists for the pull
// requests somebody remembered.
//
// A ReviewSet watches a repository and materialises one environment per open pull
// request, or per branch matching a pattern, and removes them when the pull
// request closes. The template says what kind of environment; the customer record
// says how it is built. Nothing about an individual environment is written twice.
//
// Not Runboat, and not a replacement for it. Runboat builds OCA repositories
// against upstream Odoo for the community; a ReviewSet builds THIS customer's
// own repository, with their addons and their anonymised data, on their
// database server. RunboatLink stays what it is: a window into somebody else's
// builds. See runboatlink_types.go.
//
// One thing to be explicit about, because it is a departure from how the rest of
// this operator treats credentials: the manager READS the repository token, to
// ask the forge which pull requests are open. It does not clone anything — the
// code is still only ever fetched inside the environment's own pod — but it does
// hold a token that can read repository metadata. That is the price of the
// feature, and the token wants the narrowest scope the forge offers.

// ForgeProvider is which API to ask about pull requests.
// +kubebuilder:validation:Enum=GitHub;GitLab
type ForgeProvider string

const (
	ForgeGitHub ForgeProvider = "GitHub"
	ForgeGitLab ForgeProvider = "GitLab"
)

// ReviewSetSpec describes what to watch and what to make of it.
//
// +kubebuilder:validation:XValidation:rule="has(self.watch) && (self.watch.pullRequests || size(self.watch.branches) > 0)",message="a ReviewSet that watches neither pull requests nor any branch would create nothing: set watch.pullRequests, or list watch.branches"
type ReviewSetSpec struct {
	// ForTenant is the customer these environments belong to. Their record
	// supplies the image, the database and the modules, exactly as it does for an
	// environment somebody creates by hand.
	// +kubebuilder:validation:MinLength=1
	ForTenant string `json:"forTenant"`

	// Repository is what to watch.
	Repository ReviewRepository `json:"repository"`

	// Watch says which refs become environments.
	Watch ReviewWatch `json:"watch"`

	// Template is the environment to create for each of them.
	// +optional
	Template ReviewTemplate `json:"template,omitempty"`

	// MaxEnvironments caps how many this set may have open at once.
	//
	// Separate from the customer's own quota, and below it: a repository with
	// forty open pull requests should not consume a customer's entire allowance
	// on its own, and the failure mode without this is that the fortieth pull
	// request takes staging's place.
	// +kubebuilder:default=5
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=50
	// +optional
	MaxEnvironments int32 `json:"maxEnvironments,omitempty"`

	// PollInterval is how often the forge is asked.
	//
	// Asked rather than told: a webhook needs this operator to be reachable from
	// the internet, which most clusters running a customer's ERP are not. A
	// minute is well inside every forge's rate limit for a single repository.
	// +kubebuilder:default="1m"
	// +optional
	PollInterval *metav1.Duration `json:"pollInterval,omitempty"`

	// Paused stops creating and removing without deleting the set, for the day
	// somebody needs the noise to stop and does not want to lose the
	// configuration.
	// +optional
	Paused bool `json:"paused,omitempty"`
}

// ReviewRepository is the repository and how to read it.
type ReviewRepository struct {
	// Provider is which API to ask. Declared rather than guessed from the URL:
	// self-hosted GitLab and Gitea live on their own domains.
	Provider ForgeProvider `json:"provider"`

	// URL is the repository, as you would clone it.
	// +kubebuilder:validation:MinLength=1
	URL string `json:"url"`

	// APIBase overrides the forge's API root, for self-hosted instances.
	// Empty means the public one for the provider.
	// +optional
	APIBase string `json:"apiBase,omitempty"`

	// Auth reads the repository. A public repository still needs this on GitHub
	// if you would rather not be rate-limited by IP.
	//
	// The token wants the narrowest scope the forge offers: on GitHub that is a
	// fine-grained token with pull requests read-only, and nothing else.
	// +optional
	Auth *GitAuth `json:"auth,omitempty"`
}

// ReviewWatch selects what becomes an environment.
type ReviewWatch struct {
	// PullRequests creates one environment per open pull request.
	// +optional
	PullRequests bool `json:"pullRequests,omitempty"`

	// Branches are glob patterns: "main", "release/*".
	//
	// Patterns and not a list of names, because the branches worth an
	// environment are a shape — every release branch — and enumerating them by
	// hand is the thing this exists to stop.
	// +optional
	Branches []string `json:"branches,omitempty"`

	// Labels restricts pull requests to those carrying one of these labels.
	//
	// Most pull requests do not need an environment. A label is how a person
	// says this one does, and it is a decision they can make in the tab they are
	// already looking at.
	// +optional
	Labels []string `json:"labels,omitempty"`
}

// ReviewTemplate is the environment each ref becomes.
type ReviewTemplate struct {
	// Purpose defaults to Review.
	// +kubebuilder:default=Review
	// +optional
	Purpose EnvPurpose `json:"purpose,omitempty"`

	// ImageRef names an entry in the customer's image catalogue.
	// +optional
	ImageRef string `json:"imageRef,omitempty"`

	// RepoName is the name the watched repository is given in the environment's
	// addons list. Defaults to the ReviewSet's own name.
	// +optional
	RepoName string `json:"repoName,omitempty"`

	// Paths are the subdirectories of the repository holding addons.
	// +optional
	Paths []string `json:"paths,omitempty"`

	// TTL overrides how long each environment lives. Empty follows the purpose.
	// +optional
	TTL *metav1.Duration `json:"ttl,omitempty"`
}

// ReviewSetStatus is what it found and what it made of it.
type ReviewSetStatus struct {
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// Tracked is one entry per ref that currently has an environment.
	// +listType=map
	// +listMapKey=name
	// +optional
	Tracked []TrackedRef `json:"tracked,omitempty"`

	// LastPolled is when the forge last answered.
	//
	// Recorded because "no environments appeared" has two very different causes —
	// nothing to create, or nobody asking — and they look identical from outside.
	// +optional
	LastPolled *metav1.Time `json:"lastPolled,omitempty"`

	// Message says what went wrong, in the forge's words where there are any.
	// +optional
	Message string `json:"message,omitempty"`

	// Open is how many environments this set currently has.
	// +optional
	Open int32 `json:"open,omitempty"`

	// Skipped counts refs that matched but got no environment because the cap
	// was reached. Counted rather than silently dropped: a set quietly ignoring
	// half its pull requests looks exactly like one that is working.
	// +optional
	Skipped int32 `json:"skipped,omitempty"`
}

// TrackedRef is one pull request or branch, and the environment it became.
type TrackedRef struct {
	// Name is the environment's name, which is derived from the ref.
	Name string `json:"name"`

	// Kind says which sort of ref this is.
	// +kubebuilder:validation:Enum=PullRequest;Branch
	Kind string `json:"kind"`

	// Ref is the branch, or the pull request's head branch.
	// +optional
	Ref string `json:"ref,omitempty"`

	// Number is the pull request number, when there is one.
	// +optional
	Number int32 `json:"number,omitempty"`

	// Title is the pull request's title, so the list reads like the tab the
	// person came from rather than like a list of branch names.
	// +optional
	Title string `json:"title,omitempty"`

	// URL is the pull request on the forge.
	// +optional
	URL string `json:"url,omitempty"`

	// +optional
	CreatedAt *metav1.Time `json:"createdAt,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=rset
// +kubebuilder:printcolumn:name="Customer",type=string,JSONPath=`.spec.forTenant`
// +kubebuilder:printcolumn:name="Repository",type=string,JSONPath=`.spec.repository.url`
// +kubebuilder:printcolumn:name="Open",type=integer,JSONPath=`.status.open`
// +kubebuilder:printcolumn:name="Skipped",type=integer,JSONPath=`.status.skipped`
// +kubebuilder:printcolumn:name="Polled",type=date,JSONPath=`.status.lastPolled`

// ReviewSet materialises environments from a repository's pull requests and
// branches.
type ReviewSet struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   ReviewSetSpec   `json:"spec,omitempty"`
	Status ReviewSetStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

type ReviewSetList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ReviewSet `json:"items"`
}

func init() { SchemeBuilder.Register(&ReviewSet{}, &ReviewSetList{}) }
