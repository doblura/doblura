// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Antoni Romera

package console

import (
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"

	doblurav1alpha1 "github.com/doblura/doblura/api/v1alpha1"
)

// Every string the catalogue has in one language, it has in the other.
//
// A half-translated screen is worse than an untranslated one: the reader cannot
// tell whether the English sentence is a translation nobody did or a detail that
// only exists in English, so they read it twice and trust neither.
func TestEveryStringExistsInEveryLanguage(t *testing.T) {
	var missing []string
	for id, entry := range catalogue {
		for _, l := range []locale{localeEN, localeES} {
			if strings.TrimSpace(string(entry[l])) == "" {
				missing = append(missing, id+" ["+string(l)+"]")
			}
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Fatalf("these have no text in one of the languages:\n  %s",
			strings.Join(missing, "\n  "))
	}
}

// Every state the health check can produce has words for a customer.
//
// The status page looks its sentences up by "state-"+state and "detail-"+state.
// A state nobody wrote words for renders as the literal id — "detail-gone" on a
// customer's screen — which is a bug that only shows on the day that state
// happens.
func TestEveryStateHasWordsForACustomer(t *testing.T) {
	// The states environmentHealth can set, from detail.go.
	for _, state := range []string{"up", "degraded", "down", "building", "asleep", "unknown", "gone"} {
		for _, prefix := range []string{"state-", "detail-"} {
			id := prefix + state
			if _, ok := catalogue[id]; !ok {
				t.Errorf("no %q in the catalogue: a customer in that state would be "+
					"shown the literal %q", id, id)
			}
		}
	}
}

// Every id the status template asks for exists.
//
// The template calls {{t .Locale "id"}}, and a typo there renders the id. Nothing
// else would notice, because a missing translation is not an error anywhere.
func TestTheStatusTemplateAsksForStringsThatExist(t *testing.T) {
	body, err := os.ReadFile("templates/status.html")
	if err != nil {
		t.Fatal(err)
	}
	ids := regexp.MustCompile(`\{\{t\s+\$?\.?[A-Za-z.]*\s+"([^"]+)"`).FindAllStringSubmatch(string(body), -1)
	if len(ids) == 0 {
		t.Fatal("the status template asks the catalogue for nothing at all, so it " +
			"is still hard-coded in one language")
	}
	for _, m := range ids {
		if _, ok := catalogue[m[1]]; !ok {
			t.Errorf("the template asks for %q and the catalogue has no such string, "+
				"so the page renders the id", m[1])
		}
	}
}

// The purposes are translated, and the ordering is not.
//
// They were printed raw, so a Spanish page read "prod · Production · lleva así 2
// horas" — a page translated except for the nouns, on the only screen a customer
// ever sees. And translating them in place silently changed the sort: production
// is first because it is production, not because of where its name falls in the
// alphabet of whichever language the reader has.
func TestThePurposesAreTranslatedAndTheOrderingIsNot(t *testing.T) {
	if got := purposeIn(localeES, doblurav1alpha1.PurposeProduction); got != "Producción" {
		t.Errorf("Production in Spanish is %q", got)
	}
	if got := purposeIn(localeEN, doblurav1alpha1.PurposeProduction); got != "Production" {
		t.Errorf("Production in English is %q", got)
	}
	// A purpose nobody translated shows up as itself rather than vanishing.
	if got := purposeIn(localeES, doblurav1alpha1.EnvPurpose("Sandbox")); got != "Sandbox" {
		t.Errorf("an untranslated purpose became %q", got)
	}
	if purposeIn(localeES, "") != "" {
		t.Error("no purpose must stay no purpose, not become a word")
	}
	// The rank is over the spec value in both languages.
	if purposeRank(string(doblurav1alpha1.PurposeProduction)) != 0 {
		t.Error("production is not first")
	}
	if purposeRank("Producción") == 0 {
		t.Error("the rank is being computed over a translated string, so the order " +
			"of a customer's page depends on their language")
	}
}
