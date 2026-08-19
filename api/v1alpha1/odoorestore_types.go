// SPDX-License-Identifier: Apache-2.0
// Copyright (C) 2026 Antoni Romera

package v1alpha1

import metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

// ─────────────── Putting a backup back ───────────────
//
// Taking copies and having no way to use them is half a feature, and the half
// that gets discovered on the worst day.
//
// A restore is an ACTION and not a desired state, so it is one object per
// restore rather than a field somebody edits. That shape is worth the extra kind
// for three reasons: the object records who asked and when, which is exactly what
// anybody looking back at a bad afternoon wants; a create is a thing RBAC can
// grant separately from everything else; and a spec that never changes cannot be
// re-triggered by an unrelated edit six weeks later.
//
// The acknowledgement names the ENVIRONMENT. Every other acknowledgement in this
// project is a fixed literal, and this one is not, because the mistake being
// guarded against is different: not "did you understand", but "is this the
// environment you meant". A restore YAML copied from staging to production is a
// plausible accident, and a literal that has to be retyped with the new name is
// the cheapest possible thing that stops it.

// RestorePhase is where a restore has got to.
// +kubebuilder:validation:Enum=Pending;Restoring;Succeeded;Failed
type RestorePhase string

const (
	RestorePending   RestorePhase = "Pending"
	RestoreRestoring RestorePhase = "Restoring"
	RestoreSucceeded RestorePhase = "Succeeded"
	RestoreFailed    RestorePhase = "Failed"
)

// RestoreAckFor is the literal a restore into this environment must carry.
func RestoreAckFor(environment string) string {
	return "i-accept-this-replaces-the-database-and-filestore-of-" + environment
}

// OdooRestoreSpec says which copy goes where.
//
// The whole spec is immutable. A restore that could be edited is a restore that
// can be pointed somewhere else after somebody approved it, and the object exists
// partly to be the record of what was approved.
//
// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="a restore cannot be edited once it exists: create another one. This object is the record of what was asked for, and a record that can be changed afterwards is not a record"
// +kubebuilder:validation:XValidation:rule="self.acknowledgement == 'i-accept-this-replaces-the-database-and-filestore-of-' + self.into",message="the acknowledgement must name the environment being restored INTO, exactly: 'i-accept-this-replaces-the-database-and-filestore-of-<environment>'. It names the target because a restore file copied from staging to production is a plausible accident, and retyping the name is the cheapest thing that stops it"
type OdooRestoreSpec struct {
	// Backup is the OdooBackup the copy belongs to, in this namespace.
	// +kubebuilder:validation:MinLength=1
	Backup string `json:"backup"`

	// Copy is the name of the copy, as status.copies lists it.
	// +kubebuilder:validation:MinLength=1
	Copy string `json:"copy"`

	// Into is the environment whose database and filestore are REPLACED.
	//
	// It does not have to be the environment the copy came from, and that is the
	// point: restoring production's backup into staging is how somebody
	// reproduces a bug with the data that caused it.
	// +kubebuilder:validation:MinLength=1
	Into string `json:"into"`

	// Acknowledgement must name the target environment. See the CEL message.
	// +kubebuilder:validation:MinLength=1
	Acknowledgement string `json:"acknowledgement"`

	// Neutralize disables scheduled actions, outgoing mail and payment providers
	// in the restored copy.
	//
	// Defaults to TRUE, and the default is the one that matters. A production
	// backup restored into staging with its crons live will send real invoices to
	// real customers from a machine nobody is watching. Turning it off is for
	// restoring production onto production, which is the rarer case and the one
	// worth having to say out loud.
	// +kubebuilder:default=true
	// +optional
	Neutralize *bool `json:"neutralize,omitempty"`

	// Timeout bounds the restore.
	// +kubebuilder:default="4h"
	// +optional
	Timeout *metav1.Duration `json:"timeout,omitempty"`
}

// NeutralizesRestore reports whether the restored copy is neutralized.
func (s *OdooRestoreSpec) NeutralizesRestore() bool {
	return s.Neutralize == nil || *s.Neutralize
}

// OdooRestoreStatus is how it went.
type OdooRestoreStatus struct {
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// +optional
	Phase RestorePhase `json:"phase,omitempty"`

	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// StartedAt and FinishedAt bound how long the environment was unusable,
	// which is the number somebody reports afterwards.
	// +optional
	StartedAt *metav1.Time `json:"startedAt,omitempty"`
	// +optional
	FinishedAt *metav1.Time `json:"finishedAt,omitempty"`

	// RequestedBy is who asked, taken from the authenticated identity and not
	// from anything the client sent.
	// +optional
	RequestedBy string `json:"requestedBy,omitempty"`

	// Message is what went wrong, in the words of whatever said it.
	// +optional
	Message string `json:"message,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=orst
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Copy",type=string,JSONPath=`.spec.copy`
// +kubebuilder:printcolumn:name="Into",type=string,JSONPath=`.spec.into`
// +kubebuilder:printcolumn:name="Asked by",type=string,JSONPath=`.status.requestedBy`
// +kubebuilder:printcolumn:name="Started",type=date,JSONPath=`.status.startedAt`

// OdooRestore puts one backup back into one environment.
type OdooRestore struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   OdooRestoreSpec   `json:"spec,omitempty"`
	Status OdooRestoreStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

type OdooRestoreList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []OdooRestore `json:"items"`
}

func init() { SchemeBuilder.Register(&OdooRestore{}, &OdooRestoreList{}) }
