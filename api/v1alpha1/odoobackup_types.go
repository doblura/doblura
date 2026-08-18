// SPDX-License-Identifier: Apache-2.0
// Copyright (C) 2026 Antoni Romera

package v1alpha1

import metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

// ─────────────── Backups, and how long they are kept ───────────────
//
// Not OdooSnapshot. A snapshot is an ANONYMISED copy made so that a rehearsal or
// a staging server can hold realistic data without holding customers' names; it
// is deliberately not the original, and restoring one over production would
// replace real people with invented ones.
//
// A backup is the original, kept to put back. The two are opposites in the one
// respect that matters — whether the data is still true — and giving them one
// kind would mean somebody eventually restores the wrong one.
//
// The retention is odoo.sh's, because odoo.sh's is the shape people expect from
// an Odoo host and arguing with it would only make this harder to reason about:
// seven daily, four weekly, three monthly. What that gives you is a week of
// mistakes you noticed immediately, a month of mistakes you noticed at the next
// invoice run, and a quarter of mistakes you noticed at the audit.
//
// Every backup includes the filestore. A database without its filestore restores
// to an Odoo where every attachment is a broken link, and the failure appears
// weeks later when somebody opens an invoice — which is the same failure the
// Ephemeral filestore mode exists to prevent, arriving by a different route.

// BackupRetention is how many of each are kept.
//
// +kubebuilder:validation:XValidation:rule="self.daily > 0 || self.weekly > 0 || self.monthly > 0",message="a retention policy that keeps nothing is a backup schedule that deletes everything it makes: set at least one of daily, weekly or monthly"
type BackupRetention struct {
	// +kubebuilder:default=7
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=90
	// +optional
	Daily int32 `json:"daily,omitempty"`

	// +kubebuilder:default=4
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=52
	// +optional
	Weekly int32 `json:"weekly,omitempty"`

	// +kubebuilder:default=3
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=24
	// +optional
	Monthly int32 `json:"monthly,omitempty"`
}

// OdooBackupSpec says what to copy, where to put it and how long to keep it.
//
// +kubebuilder:validation:XValidation:rule="has(self.destination.volume) || has(self.destination.objectStore)",message="a backup needs somewhere to go: set destination.volume or destination.objectStore"
type OdooBackupSpec struct {
	// Environment is what gets backed up, by name in this namespace.
	//
	// The environment rather than a database name, because a backup that does
	// not include the filestore restores to an Odoo full of broken attachments,
	// and only the environment knows where the filestore is.
	// +kubebuilder:validation:MinLength=1
	Environment string `json:"environment"`

	// Schedule is when, in cron syntax. Empty means only when asked.
	//
	// A backup nobody scheduled is a backup that exists on the day somebody
	// remembered, so this defaults to nightly rather than to nothing.
	// +kubebuilder:default="0 2 * * *"
	// +optional
	Schedule string `json:"schedule,omitempty"`

	// Retention is how many are kept.
	// +optional
	Retention BackupRetention `json:"retention,omitempty"`

	// Destination is where they go.
	Destination SnapshotDestination `json:"destination"`

	// Suspend stops the schedule without deleting it or its history.
	// +optional
	Suspend bool `json:"suspend,omitempty"`

	// Timeout bounds one run. A backup still running when the next is due is a
	// backup that will never finish and a schedule that piles up behind it.
	// +kubebuilder:default="2h"
	// +optional
	Timeout *metav1.Duration `json:"timeout,omitempty"`
}

// OdooBackupStatus is what exists and when it was made.
type OdooBackupStatus struct {
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// Copies are the backups that currently exist, newest first.
	// +optional
	Copies []BackupCopy `json:"copies,omitempty"`

	// LastSuccess is when one last completed.
	//
	// Separate from LastRun, and that separation is the point: a schedule that
	// has run every night for a week and failed every night looks busy, and the
	// only field that says otherwise is this one.
	// +optional
	LastSuccess *metav1.Time `json:"lastSuccess,omitempty"`

	// LastRun is when one last started, successful or not.
	// +optional
	LastRun *metav1.Time `json:"lastRun,omitempty"`

	// Message is what went wrong.
	// +optional
	Message string `json:"message,omitempty"`

	// Kept is how many exist right now.
	// +optional
	Kept int32 `json:"kept,omitempty"`

	// Pending are the copies the retention policy has decided to remove, which
	// the next run will delete.
	//
	// Decided by the manager and carried out by the Job, so the manager never
	// touches the volume. Visible here on purpose: a person should be able to
	// see what is about to be deleted BEFORE it is, and stop it by widening the
	// policy if the decision looks wrong.
	// +optional
	Pending []string `json:"pending,omitempty"`
}

// BackupCopy is one backup.
type BackupCopy struct {
	// Name is the file or object, which is what somebody restoring types.
	Name string `json:"name"`

	// TakenAt is when.
	TakenAt metav1.Time `json:"takenAt"`

	// Tier is which retention rule keeps it: daily, weekly or monthly.
	//
	// Recorded rather than recomputed, because a backup's tier is decided when
	// it is taken and recomputing it later would quietly reclassify — and
	// therefore quietly delete — copies that were being kept for a reason.
	// +kubebuilder:validation:Enum=daily;weekly;monthly
	// +optional
	Tier string `json:"tier,omitempty"`

	// SizeBytes as reported by the destination.
	// +optional
	SizeBytes int64 `json:"sizeBytes,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=obak
// +kubebuilder:printcolumn:name="Environment",type=string,JSONPath=`.spec.environment`
// +kubebuilder:printcolumn:name="Schedule",type=string,JSONPath=`.spec.schedule`
// +kubebuilder:printcolumn:name="Kept",type=integer,JSONPath=`.status.kept`
// +kubebuilder:printcolumn:name="Last success",type=date,JSONPath=`.status.lastSuccess`

// OdooBackup keeps copies of an environment, and removes the ones it no longer
// needs.
type OdooBackup struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   OdooBackupSpec   `json:"spec,omitempty"`
	Status OdooBackupStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

type OdooBackupList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []OdooBackup `json:"items"`
}

func init() { SchemeBuilder.Register(&OdooBackup{}, &OdooBackupList{}) }
