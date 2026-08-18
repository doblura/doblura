// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Antoni Romera

package console

import "html/template"

// Icons, inline.
//
// Not an icon font and not an emoji. A font is a network request the
// Content-Security-Policy refuses, and emoji render as whatever the reader's
// operating system decided this year — which for status glyphs means a "warning"
// that is a yellow triangle on one machine and a flat grey diamond on another.
// These are eight paths.
//
// Each state icon has a distinct SILHOUETTE, not just a distinct colour: a tick,
// a bar, a cross, a clock, a moon. That is the second of the three signals every
// state carries, and the one that survives a monochrome print or a reader who
// cannot separate the hues.
var icons = map[string]template.HTML{
	"up": `<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="M20 6 9 17l-5-5"/></svg>`,

	"degraded": `<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="M12 9v4"/><path d="M12 17h.01"/><path d="M10.3 3.9 1.8 18a2 2 0 0 0 1.7 3h17a2 2 0 0 0 1.7-3L13.7 3.9a2 2 0 0 0-3.4 0z"/></svg>`,

	"down": `<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><circle cx="12" cy="12" r="9"/><path d="m15 9-6 6"/><path d="m9 9 6 6"/></svg>`,

	"building": `<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><circle cx="12" cy="12" r="9"/><path d="M12 7v5l3 2"/></svg>`,

	"asleep": `<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="M20.5 14.6A8.5 8.5 0 1 1 9.4 3.5a7 7 0 0 0 11.1 11.1z"/></svg>`,

	"unknown": `<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><circle cx="12" cy="12" r="9"/><path d="M9.1 9a3 3 0 0 1 5.8 1c0 2-3 3-3 3"/><path d="M12 17h.01"/></svg>`,

	"gone": `<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="M3 6h18"/><path d="M8 6V4h8v2"/><path d="M6 6v14a1 1 0 0 0 1 1h10a1 1 0 0 0 1-1V6"/></svg>`,

	"customer": `<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="M3 21V7l7-4 7 4v14"/><path d="M10 21v-5h4v5"/><path d="M21 21H3"/></svg>`,

	"locked": `<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><rect x="4" y="10" width="16" height="11" rx="2"/><path d="M8 10V7a4 4 0 0 1 8 0v3"/></svg>`,
}

// stateWords are what the icon and colour say in words, which is the third
// signal and the only one that survives being read aloud.
var stateWords = map[string]string{
	"up":       "Up",
	"degraded": "Degraded",
	"down":     "Down",
	"building": "Being prepared",
	"asleep":   "Asleep",
	"gone":     "Removed",
	"unknown":  "Cannot tell",
}

func icon(name string) template.HTML { return icons[name] }
func stateWord(s string) string {
	if w, ok := stateWords[s]; ok {
		return w
	}
	return s
}
