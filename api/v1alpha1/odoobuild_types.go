// SPDX-License-Identifier: Apache-2.0
// Copyright (C) 2026 Antoni Romera

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// OdooBuild turns a set of repositories into an image.
//
// This is the piece decision 6 names, and the collision it resolves is worth
// stating at the top of the file: building means seeing the customer's SOURCE. A
// control plane that sees only metadata cannot build, so **builds run in the
// customer's own cluster** and what leaves it is "build 12 succeeded, image
// sha256:…". If a build ever runs on hardware doblura hosts, the metadata
// boundary of decision 2 is gone and nobody announced it.
//
// It is not the same thing as spec.addons.repos, and the difference is worth
// knowing before choosing:
//
//	addons.repos  clones at pod start, into an emptyDir. Nothing to publish, no
//	              registry, no credentials — and every pod pays the clone, every
//	              start can produce a different tree from a moving branch.
//	OdooBuild     produces an artefact with a digest. Slower to make, instant to
//	              start, identical everywhere, and something a rehearsal can be
//	              repeated against a year later.
//
// No Dockerfile is required from the customer, and that is deliberate: doblura
// generates it, from the base image plus the repositories declared here. A
// Dockerfile in the customer's repository is a place where the contract the base
// image guarantees — click-odoo-contrib, greenmask, the Postgres client — can be
// quietly broken, and the failure appears the day somebody needs a restore.
//
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=obuild
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Image",type=string,JSONPath=`.status.image`
// +kubebuilder:printcolumn:name="Built",type=date,JSONPath=`.status.builtAt`
// +kubebuilder:printcolumn:name="From",type=string,JSONPath=`.spec.from`,priority=1
type OdooBuild struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   OdooBuildSpec   `json:"spec,omitempty"`
	Status OdooBuildStatus `json:"status,omitempty"`
}

// OdooBuildSpec is what to build and where to put it.
//
// +kubebuilder:validation:XValidation:rule="size(self.repos) > 0",message="a build with no repositories would produce a copy of the base image under a different name"
type OdooBuildSpec struct {
	// ForTenant is the customer this image belongs to, so the console can show it
	// on their page and a quota can be attributed.
	// +optional
	ForTenant string `json:"forTenant,omitempty"`

	// From is the base image every layer is added to.
	//
	// The doblura base image by default, because that is the one whose contract
	// this operator depends on. Any image may be named; the image study is what
	// says whether it can actually be driven.
	// +kubebuilder:validation:MinLength=1
	From string `json:"from"`

	// Repos are the repositories whose addons go into the image, in the order
	// given: a later one shadows an earlier one of the same module name, which is
	// how a customer's own fix on top of an OCA module is expressed.
	//
	// USE A COMMIT for anything that has to be reproducible. A branch is resolved
	// at build time and the commit is recorded in the status, so the image can be
	// traced back — but building the same object tomorrow will produce a
	// different image, and nothing will say so at the time.
	// +kubebuilder:validation:MinItems=1
	Repos []AddonRepo `json:"repos"`

	// To is where the finished image is pushed.
	To BuildDestination `json:"to"`

	// Builder is the image that runs the build.
	//
	// Buildah, and unprivileged: measured in a real cluster as uid 1000 with
	// allowPrivilegeEscalation false and every capability dropped except SETUID
	// and SETGID, using vfs storage and chroot isolation. That set is what makes
	// this installable in a cluster with a restricted Pod Security Standard,
	// which is where anybody sensible runs an operator.
	// +kubebuilder:default="quay.io/buildah/stable:v1.37"
	// +optional
	BuilderImage string `json:"builderImage,omitempty"`

	// Size is the resource class for the build pod.
	// +kubebuilder:default=medium
	// +optional
	Size Size `json:"size,omitempty"`
}

// BuildDestination is a registry repository and the credential to write to it.
type BuildDestination struct {
	// Image is the repository, with no tag: ghcr.io/acme/erp.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([a-z0-9._:/-]*[a-z0-9])?$`
	Image string `json:"image"`

	// Tag is what the image is pushed as. Empty means one derived from the
	// resolved commits, so two builds of the same source land on the same tag and
	// a build of different source cannot overwrite it.
	//
	// A fixed tag is allowed and is a decision: it makes "latest" mean something
	// that changes underneath every environment that pulls it.
	// +optional
	Tag string `json:"tag,omitempty"`

	// PushSecret is a Secret of type kubernetes.io/dockerconfigjson with rights to
	// write to that repository.
	//
	// Required. A build that cannot be pushed produces an image that exists only
	// inside a pod that is about to be deleted, and reporting that as a success
	// would be the emptiest kind of green.
	// +kubebuilder:validation:MinLength=1
	PushSecret string `json:"pushSecret"`

	// Insecure allows pushing to a registry over plain HTTP.
	//
	// For a registry inside the cluster, which is the ordinary case for somebody
	// trying this out. Off by default: a push over HTTP to anything outside the
	// cluster sends the credential in clear.
	// +optional
	Insecure *bool `json:"insecure,omitempty"`
}

// BuildPhase summarises where a build is.
// +kubebuilder:validation:Enum=Pending;Cloning;Building;Pushing;Succeeded;Failed
type BuildPhase string

const (
	BuildPending   BuildPhase = "Pending"
	BuildCloning   BuildPhase = "Cloning"
	BuildBuilding  BuildPhase = "Building"
	BuildPushing   BuildPhase = "Pushing"
	BuildSucceeded BuildPhase = "Succeeded"
	BuildFailed    BuildPhase = "Failed"
)

// OdooBuildStatus is what came out.
type OdooBuildStatus struct {
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
	// +optional
	Phase BuildPhase `json:"phase,omitempty"`
	// +optional
	Message string `json:"message,omitempty"`

	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// Image is the full reference INCLUDING THE DIGEST, which is the only form
	// that means anything a year later. A tag is a name somebody can move.
	// +optional
	Image string `json:"image,omitempty"`

	// +optional
	BuiltAt *metav1.Time `json:"builtAt,omitempty"`

	// Sources is what each repository actually resolved to.
	//
	// The reason this exists: a build from a branch is not reproducible, and the
	// only thing that makes it traceable afterwards is a record of the commit it
	// used. Without it, "which code is in this image?" has no answer.
	// +optional
	Sources []BuiltSource `json:"sources,omitempty"`
}

// BuiltSource is one repository as it was at build time.
type BuiltSource struct {
	Name string `json:"name"`
	URL  string `json:"url"`
	// Ref is what was asked for, Commit is what that turned out to be.
	Ref    string `json:"ref"`
	Commit string `json:"commit"`
}

// +kubebuilder:object:root=true
type OdooBuildList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []OdooBuild `json:"items"`
}

func init() { SchemeBuilder.Register(&OdooBuild{}, &OdooBuildList{}) }
