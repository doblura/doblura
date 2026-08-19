// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Antoni Romera

package webhook

import (
	"context"
	"net/http"

	"gomodules.xyz/jsonpatch/v2"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	doblurav1alpha1 "github.com/doblura/doblura/api/v1alpha1"
)

// RestorerPath is where the restore-stamping webhook is served.
const RestorerPath = "/mutate-doblura-dev-v1alpha1-odoorestore"

// +kubebuilder:webhook:path=/mutate-doblura-dev-v1alpha1-odoorestore,mutating=true,failurePolicy=Fail,matchPolicy=Equivalent,sideEffects=None,timeoutSeconds=5,groups=doblura.dev,resources=odoorestores,verbs=create,versions=v1alpha1,name=restorer.odoorestore.doblura.dev,admissionReviewVersions=v1,serviceName=doblura-webhook,serviceNamespace=doblura-system

// RestoreStamp records who asked for a restore.
//
// The same reasoning as the environment's creator stamp, and more force behind
// it. A restore replaces a customer's database; the object exists to be the
// record of that, and a record with no name on it is not one.
//
// Taken from the authenticated identity and never from the object, for the
// reason that always applies to self-reported identity: a client that can write
// its own creator can write somebody else's.
//
// failurePolicy is Fail. If this can be skipped, a restore can exist with no
// name attached — and the case where somebody wants that is exactly the case
// where the name matters.
type RestoreStamp struct {
	Decoder admission.Decoder
}

func (m *RestoreStamp) Handle(_ context.Context, req admission.Request) admission.Response {
	var rs doblurav1alpha1.OdooRestore
	if err := m.Decoder.Decode(req, &rs); err != nil {
		return admission.Errored(http.StatusBadRequest, err)
	}

	// An annotation and not the status: status is written by the controller and
	// a mutating webhook cannot set it on create. The controller copies it
	// across, so the field somebody reads is on the object where they look.
	patch := jsonpatch.NewOperation("add",
		"/metadata/annotations/"+escapeJSONPointer(RequestedByAnnotation), req.UserInfo.Username)
	if rs.Annotations == nil {
		patch = jsonpatch.NewOperation("add", "/metadata/annotations",
			map[string]string{RequestedByAnnotation: req.UserInfo.Username})
	}
	return admission.Patched("recorded who asked for this restore", patch)
}

// RequestedByAnnotation carries the authenticated identity of whoever asked.
const RequestedByAnnotation = "doblura.dev/requested-by"
