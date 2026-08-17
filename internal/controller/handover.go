// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Antoni Romera

package controller

import (
	"context"
	"fmt"

	"k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	doblurav1alpha1 "github.com/doblura/doblura/api/v1alpha1"
)

// ─────────────── The multi-tenancy guardrail ───────────────
//
// The business-level equivalent of the neutralize acknowledgement: refuse to
// build a customer-facing environment from a database that holds other
// customers' data.
//
// Why this is a controller check and not a CEL rule
// ────────────────────────────────────────────────
// Every other guardrail in Doblura lives in the CRD, because a rejection at apply
// time beats an event twenty seconds later. This one cannot: answering it means
// reading a DIFFERENT object — the OdooDatabase the snapshot came from — and CEL
// validation only sees the object being admitted.
//
// There IS a validating webhook now — internal/webhook enforces the environment
// quota, which has the same "read another object" problem — and this check
// deliberately stayed where it is. The quota's answer does not change once the
// object exists, so refusing at admission is strictly better. This one's does: the
// catalogue is often filled in after the environment, and refusing at creation
// would produce a rejection indistinguishable from a typo. So it remains a
// publication gate, late on purpose, and the reason is the shape of the question
// rather than missing plumbing.

// HandoverDecision is the outcome of the check.
type HandoverDecision struct {
	Allowed bool
	// Reason is written into the object's condition, so it has to read well to
	// somebody who has never seen this code.
	Reason string
	// Database is the OdooDatabase the check consulted, when one was found.
	Database string
}

// CheckHandover decides whether data originating from sourceDatabase may be
// served to forTenant.
//
// An unknown database is allowed on purpose. Doblura must stay usable before the
// catalogue is filled in — a homelab with one hand-made dump should not have to
// declare three objects first — and refusing on missing metadata would teach
// people to work around the guardrail instead of with it. What it does instead is
// say clearly that it could not check.
func CheckHandover(
	ctx context.Context,
	c client.Client,
	namespace, sourceDatabase, forTenant string,
) (HandoverDecision, error) {
	if forTenant == "" {
		return HandoverDecision{
			Allowed: true,
			Reason:  "no target customer declared, so this is not a handover",
		}, nil
	}
	if sourceDatabase == "" {
		return HandoverDecision{
			Allowed: true,
			Reason: fmt.Sprintf(
				"cannot verify: the data source does not name an OdooDatabase, so "+
					"whether it holds customers other than %s is unknown", forTenant),
		}, nil
	}

	var db doblurav1alpha1.OdooDatabase
	err := c.Get(ctx, client.ObjectKey{Namespace: namespace, Name: sourceDatabase}, &db)
	if errors.IsNotFound(err) {
		return HandoverDecision{
			Allowed:  true,
			Database: sourceDatabase,
			Reason: fmt.Sprintf(
				"cannot verify: OdooDatabase %q does not exist, so whether it holds "+
					"customers other than %s is unknown", sourceDatabase, forTenant),
		}, nil
	}
	if err != nil {
		return HandoverDecision{}, err
	}

	ok, why := db.Spec.HandoverSafeFor(forTenant)
	return HandoverDecision{Allowed: ok, Reason: why, Database: sourceDatabase}, nil
}

// SubsettingOverrides reports whether company subsetting makes an otherwise
// unsafe handover acceptable.
//
// It is a narrow escape hatch and it is narrow on purpose. Subsetting removes
// the other customers' rows, which is necessary and not sufficient: Odoo shares
// master data — contacts, products, users — between companies by design, so the
// subset for one customer still contains partner rows that are also somebody
// else's. The caller is expected to surface that caveat, not bury it.
func SubsettingOverrides(subsetCompany string) bool {
	return subsetCompany != ""
}
