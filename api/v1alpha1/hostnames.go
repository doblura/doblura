// SPDX-License-Identifier: Apache-2.0
// Copyright (C) 2026 Antoni Romera

package v1alpha1

import (
	"crypto/rand"
	"fmt"
	"strings"
)

// Where an environment answers.
//
// Customers usually share one base domain and do not have to. Everything that is
// not production gets a name under that domain with a random tail, because a
// staging or support environment holds the customer's real data and
// "staging.acme.example" is found by anybody who tries the obvious name. The
// random part is not a secret — the ingress still asks for credentials — it is
// what stops the address being discovered by typing.

// hostAlphabet excludes the characters that are misread when somebody copies an
// address off a screen or reads it down a phone: 0/o, 1/l/i.
const hostAlphabet = "abcdefghjkmnpqrstuvwxyz23456789"

// hostSuffixLength is six, which is about a billion possibilities from this
// alphabet — far beyond guessing, and still short enough to say out loud.
const hostSuffixLength = 6

// GeneratedHost is the address for an environment with no host of its own.
//
// Returns "" when there is nothing to build from, which the caller must treat as
// "ask the person" rather than inventing a domain.
func GeneratedHost(environment, domain string) (string, error) {
	if domain == "" || environment == "" {
		return "", nil
	}
	suffix, err := randomHostSuffix()
	if err != nil {
		return "", err
	}

	// A DNS label is 63 characters. The suffix is what makes the name
	// unguessable, so the environment's name is what gets cut — losing the tail
	// would produce a predictable address, quietly.
	label := environment
	if max := 63 - hostSuffixLength - 1; len(label) > max {
		label = strings.Trim(label[:max], "-")
	}
	return label + "-" + suffix + "." + domain, nil
}

// randomHostSuffix reads from crypto/rand.
//
// Not math/rand: this is what stops somebody typing their way to a customer's
// staging data, and a predictable sequence would make every environment's address
// derivable from any other one.
func randomHostSuffix() (string, error) {
	b := make([]byte, hostSuffixLength)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generating a hostname: %w", err)
	}
	out := make([]byte, hostSuffixLength)
	for i, v := range b {
		// Modulo bias is irrelevant here: 256 mod 31 skews some characters by
		// under half a percent, against a search space of 31^6.
		out[i] = hostAlphabet[int(v)%len(hostAlphabet)]
	}
	return string(out), nil
}
