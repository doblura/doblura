// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Antoni Romera

package webhook

import (
	"context"
	"fmt"
	"net/http"

	admissionv1 "k8s.io/api/admission/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	doblurav1alpha1 "github.com/doblura/doblura/api/v1alpha1"
)

// TenantPath is where the customer-record webhook is served.
//
// It joins the existing ValidatingWebhookConfiguration rather than getting its
// own: controller-gen allows one configuration per direction, and a second
// object would need a second caBundle publisher for no benefit.
const TenantPath = "/validate-doblura-dev-v1alpha1-odootenant"

// +kubebuilder:webhook:path=/validate-doblura-dev-v1alpha1-odootenant,mutating=false,failurePolicy=Fail,matchPolicy=Equivalent,sideEffects=None,timeoutSeconds=5,groups=doblura.dev,resources=odootenants,verbs=create;update,versions=v1alpha1,name=tenant.odootenant.doblura.dev,admissionReviewVersions=v1,serviceName=doblura-webhook,serviceNamespace=doblura-system

// TenantGuard refuses a major version change that has not been rehearsed.
//
// The rule it enforces is one sentence: changing which catalogue entry is
// default WITHIN a major is an ordinary edit, and changing it ACROSS majors is
// refused unless spec.majorUpgrade names a rehearsal that actually succeeded
// against exactly that image.
//
// It lives in a webhook rather than in CEL for a reason CEL cannot work around:
// the evidence is another object. A CEL rule can see that a field says
// "rehearsal-19-final"; only something that can read the cluster can see that
// the rehearsal with that name failed, or is still running, or ran against a
// different image than the one being promoted. A gate that trusts the string is
// a gate that authorises itself.
//
// What it deliberately does NOT do is decide whether a major upgrade is a good
// idea. That is the customer's call and the consultant's judgement. It only
// insists that somebody did the work first and that the result is on record.
type TenantGuard struct {
	Decoder admission.Decoder

	// Reader is uncached, like the quota's. A rehearsal that finished seconds
	// ago is exactly the one somebody is about to cite, and a stale cache here
	// would refuse an upgrade whose evidence already exists.
	Reader client.Reader
}

func (t *TenantGuard) Handle(ctx context.Context, req admission.Request) admission.Response {
	var tenant doblurav1alpha1.OdooTenant
	if err := t.Decoder.Decode(req, &tenant); err != nil {
		return admission.Errored(http.StatusBadRequest, err)
	}

	// Only one customer record can be the default one.
	//
	// Refused HERE, when the second one is marked, rather than later when somebody
	// creates an environment: the person marking it is the person who can fix it,
	// and they are looking at the object right now. Refusing at environment time
	// would surface it to whoever happened to create the next environment, about a
	// record they may not have touched.
	if tenant.Spec.IsDefault() {
		if other, err := t.otherDefault(ctx, &tenant); err != nil {
			return admission.Errored(http.StatusInternalServerError, err)
		} else if other != "" {
			return admission.Denied(fmt.Sprintf(
				"%s is already the default customer in %s, and there can only be "+
					"one: with two, which defaults an environment inherits would "+
					"depend on the order they came back in. Unmark %s first",
				other, tenant.Namespace, other))
		}
	}

	next := tenant.Spec.DefaultImage()
	if next == nil {
		// No catalogue, or no entry marked default in a catalogue of several.
		// Nothing to guard: environments fall back to environmentDefaults.image
		// and the customer has not expressed a version here at all.
		return admission.Allowed("no default catalogue entry")
	}

	// On CREATE there is nothing to cross FROM. A new customer starting on 19 is
	// not upgrading; refusing it would demand a rehearsal of a migration that is
	// not happening.
	if req.Operation == admissionv1.Create {
		return admission.Allowed("new customer record")
	}

	var old doblurav1alpha1.OdooTenant
	if err := t.Decoder.DecodeRaw(req.OldObject, &old); err != nil {
		return admission.Errored(http.StatusBadRequest, err)
	}
	prev := old.Spec.DefaultImage()
	if prev == nil || prev.Major() == "" || next.Major() == prev.Major() {
		return admission.Allowed("no change of major version")
	}

	up := tenant.Spec.MajorUpgrade
	if up == nil {
		return admission.Denied(fmt.Sprintf(
			"this changes %s from Odoo %s to Odoo %s, which rewrites the database in place "+
				"and cannot be rolled back by redeploying the old image. Set spec.majorUpgrade "+
				"with toImage %q, the name of an OdooRehearsal in this namespace that succeeded "+
				"against %s, and the acknowledgement. Changing between builds of the same major "+
				"needs none of this.",
			tenant.Name, prev.OdooVersion, next.OdooVersion, next.Name, next.Image))
	}
	if up.ToImage != next.Name {
		return admission.Denied(fmt.Sprintf(
			"spec.majorUpgrade.toImage is %q but the entry being promoted is %q. This is "+
				"refused rather than ignored: an authorisation left behind from a previous "+
				"upgrade would otherwise wave through the next one",
			up.ToImage, next.Name))
	}

	var reh doblurav1alpha1.OdooRehearsal
	err := t.Reader.Get(ctx, client.ObjectKey{Namespace: tenant.Namespace, Name: up.RehearsalRef}, &reh)
	switch {
	case errors.IsNotFound(err):
		return admission.Denied(fmt.Sprintf(
			"no OdooRehearsal named %q in namespace %s. The evidence for a major upgrade is "+
				"an object this webhook can read, not a field you fill in",
			up.RehearsalRef, tenant.Namespace))
	case err != nil:
		return admission.Errored(http.StatusInternalServerError, err)
	}

	if reh.Status.Phase != doblurav1alpha1.PhaseSucceeded {
		return admission.Denied(fmt.Sprintf(
			"rehearsal %q is %s, not Succeeded. %s",
			up.RehearsalRef, phaseOrPending(reh.Status.Phase), reh.Status.Message))
	}
	if reh.Spec.Image != next.Image {
		return admission.Denied(fmt.Sprintf(
			"rehearsal %q ran against %s, but this promotes %s. A migration rehearsed on a "+
				"different image is not evidence about this one",
			up.RehearsalRef, reh.Spec.Image, next.Image))
	}

	return admission.Allowed(fmt.Sprintf(
		"major upgrade to Odoo %s authorised by rehearsal %s", next.OdooVersion, up.RehearsalRef))
}

func phaseOrPending(p doblurav1alpha1.RehearsalPhase) string {
	if p == "" {
		return "Pending (it has not started)"
	}
	return string(p)
}

// otherDefault is another customer record already marked as the default one.
//
// Uncached, like everything else this webhook reads: a record marked a second ago
// is exactly the one being duplicated.
func (t *TenantGuard) otherDefault(
	ctx context.Context,
	tenant *doblurav1alpha1.OdooTenant,
) (string, error) {
	var list doblurav1alpha1.OdooTenantList
	if err := t.Reader.List(ctx, &list, client.InNamespace(tenant.Namespace)); err != nil {
		return "", fmt.Errorf("looking for an existing default customer: %w", err)
	}
	for i := range list.Items {
		other := &list.Items[i]
		if other.Name == tenant.Name || !other.Spec.IsDefault() {
			continue
		}
		return other.Name, nil
	}
	return "", nil
}
