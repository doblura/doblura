// SPDX-License-Identifier: Apache-2.0
// Copyright (C) 2026 Antoni Romera

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// OdooRelease moves one version of YOUR OWN PRODUCT across many customers.
//
// The kind an integrator needs and a single-Odoo company does not: thirty
// customers on a product you maintain, a new version of it, and the question of
// how it reaches them without either thirty manual afternoons or one afternoon
// that breaks thirty customers at once.
//
// # WHAT IT DOES AND WHAT IT DELIBERATELY DOES NOT
//
// It moves a customer onto the release: the image goes into their catalogue and
// their record says which version they are on. From then on, every environment
// they open runs it.
//
// It does NOT reach into a running Production environment and change its image.
// That is a migration against live data, and this operator exists precisely
// because that is the thing you rehearse and then decide, with somebody watching.
// An operator that did it on a timer would be automating away the only step that
// was ever worth a person's attention. When a customer is ready, moving their
// production is one edit, made by somebody who chose the moment.
//
// THE THREE THINGS THAT KEEP IT FROM BEING A FLEET-WIDE ACCIDENT
//
//   - The selector. A customer is in scope because their record carries a label
//     somebody put there. There is no "all customers" mode.
//   - The rehearsal. A customer is not moved until a rehearsal OF THEIR DATA
//     against this exact image has passed. That is the whole thesis of this
//     operator applied to itself.
//   - The batch. A few at a time, with a soak between them, and the rollout stops
//     the moment one of them ends up in a bad state rather than carrying on into
//     the next batch.
//
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=orel
// +kubebuilder:printcolumn:name="Version",type=string,JSONPath=`.spec.version`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="On it",type=integer,JSONPath=`.status.onThisRelease`
// +kubebuilder:printcolumn:name="Waiting",type=integer,JSONPath=`.status.waiting`
// +kubebuilder:printcolumn:name="Next batch",type=string,JSONPath=`.status.nextBatchAt`
type OdooRelease struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   OdooReleaseSpec   `json:"spec,omitempty"`
	Status OdooReleaseStatus `json:"status,omitempty"`
}

// OdooReleaseSpec is the version, who gets it, and how fast.
//
// +kubebuilder:validation:XValidation:rule="has(self.image) != has(self.fromBuild)",message="a release names an image OR the build that produces it, and exactly one"
// +kubebuilder:validation:XValidation:rule="has(self.selector.matchLabels) || has(self.selector.matchExpressions)",message="a release needs a selector that selects something: an empty one means every customer in this namespace, which is the accident this kind is shaped to prevent"
// +kubebuilder:validation:XValidation:rule="!has(self.requireRehearsal) || self.requireRehearsal || self.unrehearsedAcknowledgement == 'i-accept-moving-customers-onto-a-version-nobody-rehearsed'",message="turning requireRehearsal off removes the only thing that distinguishes this from `kubectl set image` across every customer at once, so it needs unrehearsedAcknowledgement set to its literal value"
type OdooReleaseSpec struct {
	// Version is what this release is called in your own numbering. It is written
	// onto each customer's record as they move, so "who is on what" is a question
	// with an answer.
	// +kubebuilder:validation:MinLength=1
	Version string `json:"version"`

	// Image is the artifact, and a DIGEST is the only form that means anything: a
	// rollout that took a week and a tag that moved on Tuesday is a rollout where
	// half the customers are on something nobody chose.
	// +optional
	Image string `json:"image,omitempty"`

	// FromBuild names an OdooBuild instead, and takes the digest it produced.
	// +optional
	FromBuild string `json:"fromBuild,omitempty"`

	// Selector is which customers are in scope, and it is the opt-in.
	//
	// There is deliberately no "everybody" — not as a default, not as an empty
	// selector meaning all. A rollout that reaches a customer nobody added to it
	// is the accident this kind is shaped to prevent, and an empty selector is
	// how every other Kubernetes API spells "everybody".
	Selector metav1.LabelSelector `json:"selector"`

	// Batch is how many customers move at once, and how long to wait before the
	// next few.
	// +optional
	Batch BatchPolicy `json:"batch,omitempty"`

	// RequireRehearsal gates each customer on a rehearsal of THEIR data against
	// this exact image.
	//
	// Turning it off needs the acknowledgement below, because it removes the only
	// thing that distinguishes this from a `kubectl set image` across thirty
	// customers.
	// +kubebuilder:default=true
	// +optional
	RequireRehearsal *bool `json:"requireRehearsal,omitempty"`

	// UnrehearsedAcknowledgement is required, literally, to turn the gate off.
	// +optional
	UnrehearsedAcknowledgement string `json:"unrehearsedAcknowledgement,omitempty"`
}

// BatchPolicy is the pace.
//
// +kubebuilder:validation:XValidation:rule="!has(self.size) || self.size >= 1",message="a batch of zero customers never moves anybody"
type BatchPolicy struct {
	// Size is how many customers move in one go.
	//
	// Small by default, and this is a real default rather than a placeholder: the
	// point of batching is that the first few customers are the ones who find out
	// what the release does to real data, and a batch big enough to be efficient
	// is a batch big enough to make that discovery expensive.
	// +kubebuilder:default=3
	// +optional
	Size *int32 `json:"size,omitempty"`

	// Soak is how long to leave a batch alone before starting the next.
	//
	// A day by default. Most of what a bad release does to an Odoo shows up when
	// somebody uses it, not when it starts, so an hour proves the pods are up and
	// very little else.
	// +kubebuilder:default="24h"
	// +optional
	Soak *metav1.Duration `json:"soak,omitempty"`
}

// ReleasePhase summarises a rollout in one word.
// +kubebuilder:validation:Enum=Pending;Rolling;Soaking;Blocked;Complete
type ReleasePhase string

const (
	ReleasePending  ReleasePhase = "Pending"
	ReleaseRolling  ReleasePhase = "Rolling"
	ReleaseSoaking  ReleasePhase = "Soaking"
	ReleaseBlocked  ReleasePhase = "Blocked"
	ReleaseComplete ReleasePhase = "Complete"
)

// OdooReleaseStatus is where the rollout has got to.
type OdooReleaseStatus struct {
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
	// +optional
	Phase ReleasePhase `json:"phase,omitempty"`
	// +optional
	Message string `json:"message,omitempty"`

	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// Image is what this release resolved to, recorded so a rollout that spanned
	// a week can be read afterwards without wondering whether the tag moved.
	// +optional
	Image string `json:"image,omitempty"`

	// InScope, OnThisRelease and Waiting are the three numbers somebody wants.
	// +optional
	InScope int32 `json:"inScope,omitempty"`
	// +optional
	OnThisRelease int32 `json:"onThisRelease,omitempty"`
	// +optional
	Waiting int32 `json:"waiting,omitempty"`

	// NextBatchAt is when the soak ends. Empty when nothing is waiting on time.
	// +optional
	NextBatchAt *metav1.Time `json:"nextBatchAt,omitempty"`

	// LastMovedAt is when the most recent customer was moved, which is what the
	// soak counts from.
	// +optional
	LastMovedAt *metav1.Time `json:"lastMovedAt,omitempty"`

	// Customers is each one in scope and where it stands.
	//
	// Every customer, not only the interesting ones: a rollout list that shows
	// what moved and hides what did not is a list that reads as finished.
	// +optional
	Customers []ReleaseCustomer `json:"customers,omitempty"`
}

// ReleaseCustomer is one customer's position in a rollout.
type ReleaseCustomer struct {
	Name string `json:"name"`
	// State is one of: onRelease, rehearsing, notRehearsed, waitingForBatch,
	// blocked.
	State string `json:"state"`
	// Why is the sentence for anything that is not simply "on it".
	// +optional
	Why string `json:"why,omitempty"`
	// MovedAt is when this customer was put on the release.
	// +optional
	MovedAt *metav1.Time `json:"movedAt,omitempty"`
}

// +kubebuilder:object:root=true
type OdooReleaseList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []OdooRelease `json:"items"`
}

func init() { SchemeBuilder.Register(&OdooRelease{}, &OdooReleaseList{}) }
