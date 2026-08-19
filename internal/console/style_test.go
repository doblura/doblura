// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Antoni Romera

package console

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// The templates and the stylesheet are written in different files by the same
// person on different days, and nothing connected them.
//
// Twice now a template has shipped with a class the stylesheet does not define:
// a quota meter that was permanently empty, and a panel button four pixels from
// the container above it. Both looked deliberate. Neither was caught by anything,
// because a missing CSS class is not an error anywhere — the element simply
// renders unstyled, which is a state the page also has legitimately.
//
// So the check that was being done by eye is done here.

var (
	classAttr      = regexp.MustCompile(`class="([^"]*)"`)
	templateAction = regexp.MustCompile(`\{\{[\s\S]*?\}\}`)
)

// templateClasses is every class name the templates use, with the Go template
// actions stripped out.
func templateClasses(t *testing.T) map[string][]string {
	t.Helper()
	out := map[string][]string{}

	files, err := filepath.Glob("templates/*.html")
	if err != nil || len(files) == 0 {
		t.Fatalf("no templates found: %v", err)
	}
	for _, f := range files {
		body, err := os.ReadFile(f) //nolint:gosec // a fixed glob in the package
		if err != nil {
			t.Fatal(err)
		}
		// Template actions come out of the WHOLE file first, not out of each
		// attribute. An action can contain a quote — {{if eq .Mode "none"}} —
		// which ends the class attribute early and turns the rest of the
		// expression into fake class names. The first version of this test
		// reported `$l.Mode` and `(eq` as missing styles, which is the test
		// failing rather than the stylesheet.
		text := templateAction.ReplaceAllString(string(body), " ")

		for _, m := range classAttr.FindAllStringSubmatch(text, -1) {
			for _, c := range strings.Fields(m[1]) {
				// A name completed at render time — state-{{.State}} leaves
				// "state-" — is checked as a PREFIX: some selector must start
				// with it, or every value it can take is unstyled.
				if strings.HasSuffix(c, "-") {
					out[c] = append(out[c], filepath.Base(f))
					continue
				}
				out[c] = append(out[c], filepath.Base(f))
			}
		}
	}
	return out
}

func TestEveryClassInATemplateIsStyled(t *testing.T) {
	css, err := os.ReadFile("assets/console.css")
	if err != nil {
		t.Fatal(err)
	}
	sheet := string(css)

	// Classes whose styling comes from somewhere else, listed so that adding one
	// is a decision somebody writes down rather than a silent gap.
	known := map[string]bool{
		// state-<word> and s-<word> are composed from a value at render time and
		// their bases are checked instead.
		"state": true, "push": true,
	}

	used := templateClasses(t)
	var missing []string
	for class, files := range used {
		if known[class] {
			continue
		}
		// A prefix completed at render time only needs SOMETHING starting with
		// it; the exact values come from Go and are checked by their own tests.
		if strings.HasSuffix(class, "-") {
			if strings.Contains(sheet, "."+class) {
				continue
			}
			missing = append(missing, class+"* (in "+strings.Join(unique(files), ", ")+")")
			continue
		}
		// Matched anywhere in a selector: .btn.ghost, button.danger and
		// details.panel.inline are all legitimate ways to style one.
		if regexp.MustCompile(`\.` + regexp.QuoteMeta(class) + `(?:[^\w-]|$)`).MatchString(sheet) {
			continue
		}
		missing = append(missing, class+" (in "+strings.Join(unique(files), ", ")+")")
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Fatalf("these classes are used and never styled, so the elements "+
			"wearing them render unstyled and look deliberate:\n  %s",
			strings.Join(missing, "\n  "))
	}
}

// Nothing carries an inline style, because the CSP silently drops it.
//
// The console serves `style-src 'self'` with no unsafe-inline, so a style="..."
// attribute is removed by the browser without a word. It shipped twice: a meter
// that was always empty and a pill in the wrong place, both of which looked like
// design decisions.
func TestNoTemplateUsesAnInlineStyle(t *testing.T) {
	files, _ := filepath.Glob("templates/*.html")
	var offenders []string
	for _, f := range files {
		body, err := os.ReadFile(f) //nolint:gosec // a fixed glob in the package
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(body), "style=\"") {
			offenders = append(offenders, filepath.Base(f))
		}
	}
	if len(offenders) > 0 {
		t.Fatalf("inline styles in %s: the Content-Security-Policy drops them "+
			"without a word, so whatever they were for silently does not happen",
			strings.Join(offenders, ", "))
	}
}

func unique(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, v := range in {
		if !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}
	sort.Strings(out)
	return out
}
