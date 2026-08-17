// SPDX-License-Identifier: Apache-2.0
// Copyright (C) 2026 Antoni Romera

package v1alpha1

import (
	"fmt"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ─────────────── Where databases live ───────────────
//
// An OdooInstance is a Postgres server Doblura may place databases on. It is a
// catalogue entry, not something Doblura provisions: bring your own Postgres,
// however you run it.
//
// It exists for two reasons that only show up at scale:
//
//  1. Placement. With three customers you assign databases by hand. With ten
//     customers and five environments each that is fifty databases, and the
//     question "where does this one go?" gets answered badly — always on the
//     instance somebody remembers, until it fills up at 3am.
//
//  2. The Postgres version. The first real end-to-end run lost twenty minutes
//     to a pg18 client restoring into a pg16 server: the client emits
//     `SET transaction_timeout`, the server rejects it, and the tooling reports
//     it as a bare "Couldn't restore database". Recording the observed server
//     version here lets the operator warn about that before running anything.

// InstanceTier says which kind of workload may land on an instance.
//
// The separation is the whole point of having tiers: a rehearsal restoring
// forty gigabytes should not be competing for I/O with a customer who is
// invoicing. Enforcing it here means nobody has to remember it.
// +kubebuilder:validation:Enum=Production;NonProduction;Any
type InstanceTier string

const (
	// TierProduction accepts production databases only.
	TierProduction InstanceTier = "Production"

	// TierNonProduction accepts everything that is not production: staging, QA,
	// review environments, rehearsal scratch databases.
	TierNonProduction InstanceTier = "NonProduction"

	// TierAny accepts anything. Reasonable for a homelab, a bad idea once you
	// have customers: see the comment on InstanceTier.
	TierAny InstanceTier = "Any"
)

// InstanceCapacity bounds what may be placed on an instance.
type InstanceCapacity struct {
	// MaxDatabases is the hard ceiling on databases placed by Doblura.
	//
	// It counts what Doblura placed, not what the server holds: a server shared
	// with something else is still your responsibility to size.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:default=20
	// +optional
	MaxDatabases int32 `json:"maxDatabases,omitempty"`

	// ReservedGi is disk to keep free, in GiB. Placement refuses an instance
	// whose free space would drop below it.
	//
	// Reserving headroom is not caution, it is arithmetic: a rehearsal stages a
	// writable copy of the dump, so restoring a 40 GiB database needs 40 GiB of
	// scratch on top of the 40 GiB it restores into.
	// +optional
	ReservedGi *int32 `json:"reservedGi,omitempty"`
}

// InstanceClass is how a Postgres server is operated.
//
// It exists because the two are not interchangeable in cost. Measured on published
// EU provider prices, an environment-hour on an in-cluster CloudNativePG instance
// and one on a managed instance in a named region differ by up to an order of
// magnitude — and until now the product could not tell them apart, because
// InstanceTier records Production/NonProduction and nothing else.
//
// Doblura provisions neither. This records which one you brought.
// +kubebuilder:validation:Enum=InCluster;Managed;Unknown
type InstanceClass string

const (
	// ClassInCluster is Postgres you run yourself, on this cluster or another.
	// Usually CloudNativePG. The cheap default, and the one the self-hosted path
	// assumes.
	ClassInCluster InstanceClass = "InCluster"

	// ClassManaged is somebody else's operated service: RDS, Cloud SQL, a
	// provider's managed Postgres. Costs more, and is the honest answer when a
	// customer requires a named region, a signed DPA or auditable PITR.
	ClassManaged InstanceClass = "Managed"

	// ClassUnknown is the default, and it is deliberately not InCluster.
	//
	// Guessing here would put a number on an invoice that nobody chose. An
	// unclassified instance should show up as unclassified.
	ClassUnknown InstanceClass = "Unknown"
)

// OdooInstanceSpec is a Postgres server Doblura may place databases on.
type OdooInstanceSpec struct {
	// +kubebuilder:validation:MinLength=1
	Host string `json:"host"`

	// +kubebuilder:default=5432
	// +optional
	Port int32 `json:"port,omitempty"`

	// AdminUser needs CREATEDB, and DROPDB for ephemeral workloads. It does not
	// need superuser.
	// +kubebuilder:validation:MinLength=1
	AdminUser string `json:"adminUser"`

	// AdminPasswordSecret is the Secret holding the "password" key.
	// +kubebuilder:validation:MinLength=1
	AdminPasswordSecret string `json:"adminPasswordSecret"`

	// +kubebuilder:default=NonProduction
	// +optional
	Tier InstanceTier `json:"tier,omitempty"`

	// +kubebuilder:default={}
	// +optional
	Capacity InstanceCapacity `json:"capacity,omitempty"`

	// Class is how this server is operated. See InstanceClass: it is what lets
	// usage be rated, because the cost of an hour depends on it.
	// +kubebuilder:default=Unknown
	// +optional
	Class InstanceClass `json:"class,omitempty"`

	// Region is where it physically is, in the provider's own naming —
	// "eu-central-1", "fsn1", "es-mad". Free text on purpose: normalising the
	// world's region names is a losing game, and the value that matters is the one
	// that appears on the provider's invoice.
	//
	// It is recorded rather than enforced. Nothing here checks that a database
	// placed on it is allowed to be in that region; that is a data-protection
	// decision, and pretending a string comparison settles it would be worse than
	// leaving it visible.
	// +kubebuilder:validation:MaxLength=64
	// +optional
	Region string `json:"region,omitempty"`

	// ObserveInterval is how often to probe the server.
	//
	// The probe is a short-lived Pod, not a connection from the manager: see the
	// comment on the OdooInstance controller for why that choice was made and what
	// it costs.
	// +kubebuilder:default="10m"
	// +optional
	ObserveInterval *metav1.Duration `json:"observeInterval,omitempty"`

	// Unschedulable takes the instance out of placement without deleting it.
	// The databases already on it keep working; nothing new lands.
	//
	// The operational equivalent of cordoning a node, and it is the field you
	// will actually use: draining an instance before maintenance.
	// +optional
	Unschedulable *bool `json:"unschedulable,omitempty"`
}

// OdooInstanceStatus is written by the controller only.
type OdooInstanceStatus struct {
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// ServerVersion as reported by the server.
	//
	// This is the field that pays for the whole type. Your image's Postgres
	// client must be compatible with it, and a newer client against an older
	// server produces SQL the server rejects. Having it here lets the operator
	// say so before a restore fails obscurely.
	// +optional
	ServerVersion string `json:"serverVersion,omitempty"`

	// Databases is how many databases Doblura currently has placed here.
	// +optional
	Databases int32 `json:"databases,omitempty"`

	// Available is MaxDatabases minus Databases. Surfaced so `kubectl get` shows
	// where there is room without arithmetic.
	// +optional
	Available int32 `json:"available,omitempty"`

	// DiskFreeGi and DiskTotalGi are the data directory's filesystem, in GiB.
	//
	// These are what make capacity.reservedGi mean anything. Until they existed the
	// field was documented as enforced and read by nothing — a number on the
	// customer record that an operator believes bounds the cluster, and does not.
	// +optional
	DiskFreeGi *int32 `json:"diskFreeGi,omitempty"`

	// +optional
	DiskTotalGi *int32 `json:"diskTotalGi,omitempty"`

	// LastProbe is when the observations above were taken.
	//
	// The most important field in this status, for the same reason as on
	// RunboatLink: everything else here is a measurement, and a measurement is
	// worth what its age says it is. Placement refuses an instance it has never
	// observed rather than assuming the disk is empty.
	// +optional
	LastProbe *metav1.Time `json:"lastProbe,omitempty"`

	// +optional
	Message string `json:"message,omitempty"`
}

// Instance condition types.
const (
	// ConditionReachable is whether the operator could connect. An unreachable
	// instance is skipped by placement rather than failing it: one server being
	// down should not stop a rehearsal that could run elsewhere.
	ConditionReachable = "Reachable"
	// ConditionSchedulable is whether placement may use it: reachable, not
	// cordoned, and with capacity left.
	ConditionSchedulable = "Schedulable"
)

// OdooInstance is a Postgres server Doblura may place Odoo databases on.
//
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=oinst
// +kubebuilder:printcolumn:name="Host",type=string,JSONPath=`.spec.host`
// +kubebuilder:printcolumn:name="Tier",type=string,JSONPath=`.spec.tier`
// +kubebuilder:printcolumn:name="PG",type=string,JSONPath=`.status.serverVersion`
// +kubebuilder:printcolumn:name="DBs",type=integer,JSONPath=`.status.databases`
// +kubebuilder:printcolumn:name="Free",type=integer,JSONPath=`.status.available`
// +kubebuilder:printcolumn:name="Schedulable",type=string,JSONPath=`.status.conditions[?(@.type=="Schedulable")].status`
type OdooInstance struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   OdooInstanceSpec   `json:"spec,omitempty"`
	Status OdooInstanceStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// OdooInstanceList is a list of OdooInstance.
type OdooInstanceList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []OdooInstance `json:"items"`
}

func init() {
	SchemeBuilder.Register(&OdooInstance{}, &OdooInstanceList{})
}

// AcceptsTier reports whether this instance may host a database of the given
// role.
//
// Production databases only land on Production or Any instances; everything
// else only on NonProduction or Any. The asymmetry is deliberate: an
// unclassified instance should not silently become production's neighbour.
func (s *OdooInstanceSpec) AcceptsRole(role DatabaseRole) bool {
	if s.Tier == TierAny {
		return true
	}
	if role == RoleProduction {
		return s.Tier == TierProduction
	}
	return s.Tier == TierNonProduction
}

// ObserveEvery returns the probe interval, defaulted and floored.
//
// The floor exists for the same reason RunboatLink has one: each probe is a Pod,
// and one badly-configured instance should not be able to fill a node with them.
func (s *OdooInstanceSpec) ObserveEvery() metav1.Duration {
	const (
		def   = 10 * time.Minute
		floor = time.Minute
	)
	if s.ObserveInterval == nil || s.ObserveInterval.Duration <= 0 {
		return metav1.Duration{Duration: def}
	}
	if s.ObserveInterval.Duration < floor {
		return metav1.Duration{Duration: floor}
	}
	return *s.ObserveInterval
}

// HeadroomFor reports whether placing a database of the given size would leave
// less free disk than capacity.reservedGi demands, and why when it would.
//
// Three answers, not two, and the third is the one that matters:
//
//	(true,  "")     there is room
//	(false, reason) there is not
//	(false, reason) the disk has never been observed, so nobody knows
//
// The third case refuses. That is the whole point of the field: an instance whose
// free space is unknown is exactly the instance you must not fill, and defaulting
// an unobserved disk to "empty" would turn a safety limit into a decoration —
// which is what this field was for its entire life before the probe existed.
func (i *OdooInstance) HeadroomFor(sizeGi *int32) (bool, string) {
	return headroomFor(i.Spec.Capacity, i.Status, sizeGi)
}

// HeadroomFor is the same check from the placer's point of view, which holds the
// spec and status side by side rather than a whole object.
func (c *PlacementCandidate) HeadroomFor(sizeGi *int32) (bool, string) {
	return headroomFor(c.Spec.Capacity, c.Status, sizeGi)
}

func headroomFor(cap InstanceCapacity, st OdooInstanceStatus, sizeGi *int32) (bool, string) {
	reserved := cap.ReservedGi
	if reserved == nil || *reserved <= 0 {
		return true, "" // no reservation declared: nothing to enforce
	}
	if st.LastProbe == nil || st.DiskFreeGi == nil {
		return false, "capacity.reservedGi is set but the disk has never been " +
			"observed; placement will not guess (is the manager able to run a probe Pod?)"
	}

	want := int32(0)
	if sizeGi != nil && *sizeGi > 0 {
		// A restore of an OdooBackup stages a writable copy of the dump, so it
		// needs the database's size AGAIN as scratch. Reserving only the final
		// size is the arithmetic error the field's own comment warns about.
		want = *sizeGi * 2
	}

	after := *st.DiskFreeGi - want
	if after < *reserved {
		return false, fmt.Sprintf(
			"would leave %dGi free, below the reserved %dGi (%dGi free now, "+
				"%dGi needed including scratch)",
			after, *reserved, *st.DiskFreeGi, want)
	}
	return true, ""
}
