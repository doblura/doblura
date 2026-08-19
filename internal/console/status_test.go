// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Antoni Romera

package console

import (
	"strings"
	"testing"
)

// The customer's page must not name things the customer cannot see.
//
// This exists because the first version passed the operator's health detail
// straight through, and a customer looking at a broken Production environment was
// told to "check the logs of Job prod-sinbackup-migrate": an object they cannot
// list, a permission they do not have, and a sentence that reads as a second
// fault. The words on that page are now written for its reader, and this keeps
// them that way.
func TestTheCustomerPageSpeaksToTheCustomer(t *testing.T) {
	// Words that mean something to whoever runs the platform and nothing to the
	// person whose Odoo it is.
	operatorWords := []string{
		"Job", "pod", "phase", "kubectl", "Deployment", "replica",
		"namespace", "reconcile", "CronJob", "container",
	}

	for _, state := range []string{"up", "degraded", "down", "building", "asleep", "unknown"} {
		for _, text := range []string{customerWord(state), customerDetail(state)} {
			if text == "" {
				t.Fatalf("state %q has no words at all, so the page would show a "+
					"blank line where the answer goes", state)
			}
			for _, w := range operatorWords {
				if strings.Contains(text, w) {
					t.Errorf("state %q says %q, which names %q — the reader of this "+
						"page cannot see that and cannot act on it", state, text, w)
				}
			}
		}
	}

	// And the headline has to be a sentence somebody would say, not a status word.
	if got := customerWord("degraded"); got == "Degraded" {
		t.Error("the headline is monitoring vocabulary rather than an answer")
	}
	if !strings.Contains(customerDetail("down"), "tell them") {
		t.Error("the down message does not tell the reader what to do, which is the " +
			"only action available to them")
	}
}
