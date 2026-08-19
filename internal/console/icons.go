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

	// ── navigation ──
	//
	// One distinct SILHOUETTE each, because the rail collapses to icons alone and
	// at that point the shape is the only thing left. Two icons that differ only
	// in a detail are two icons nobody can tell apart at 17 pixels.
	"nav-overview": `<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.9" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><rect x="3" y="3" width="7" height="9" rx="1.5"/><rect x="14" y="3" width="7" height="5" rx="1.5"/><rect x="14" y="12" width="7" height="9" rx="1.5"/><rect x="3" y="16" width="7" height="5" rx="1.5"/></svg>`,

	"nav-customers": `<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.9" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="M3 21V7l7-4 7 4v14"/><path d="M10 21v-5h4v5"/><path d="M21 21H3"/><path d="M7 9h.01M7 13h.01M13 9h.01M13 13h.01"/></svg>`,

	"nav-environments": `<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.9" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="m12 2 9 5v10l-9 5-9-5V7z"/><path d="m3 7 9 5 9-5"/><path d="M12 12v10"/></svg>`,

	"nav-rehearsals": `<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.9" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="M9 3h6"/><path d="M10 3v6.5L4.5 19a2 2 0 0 0 1.7 3h11.6a2 2 0 0 0 1.7-3L14 9.5V3"/><path d="M7.5 15h9"/></svg>`,

	// A safe, distinct from the stacked sheets that mean a snapshot: the two are
	// opposites — one is the original kept to put back, the other an anonymised
	// copy — and two similar icons would blur exactly the distinction that
	// matters.
	"nav-backups": `<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.9" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><rect x="3" y="4" width="18" height="16" rx="2"/><circle cx="12" cy="12" r="3.4"/><path d="M12 8.6V12l2.2 1.6"/></svg>`,

	"nav-snapshots": `<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.9" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="m12 2 9 4.5-9 4.5-9-4.5z"/><path d="m3 12 9 4.5 9-4.5"/><path d="m3 17.5 9 4.5 9-4.5"/></svg>`,

	"nav-databases": `<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.9" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><ellipse cx="12" cy="5.5" rx="8" ry="3.2"/><path d="M4 5.5v13c0 1.8 3.6 3.2 8 3.2s8-1.4 8-3.2v-13"/><path d="M4 12c0 1.8 3.6 3.2 8 3.2s8-1.4 8-3.2"/></svg>`,

	"nav-servers": `<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.9" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><rect x="2.5" y="3" width="19" height="7" rx="1.6"/><rect x="2.5" y="14" width="19" height="7" rx="1.6"/><path d="M6.5 6.5h.01M6.5 17.5h.01"/><path d="M11 6.5h6M11 17.5h6"/></svg>`,

	// Two people, for the page about who has access — distinct from nav-access,
	// which is a key and means "what YOU can do".
	// Stacked layers, for the cluster picker.
	// Half-filled circle: the usual sign for a theme, and it reads at 17px.
	"nav-theme":   `<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.9" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><circle cx="12" cy="12" r="8.5"/><path d="M12 3.5a8.5 8.5 0 0 1 0 17z" fill="currentColor" stroke="none"/></svg>`,
	"nav-cluster": `<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.9" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="m12 3 8.5 4.2L12 11.4 3.5 7.2z"/><path d="m3.5 12 8.5 4.2 8.5-4.2"/><path d="m3.5 16.6 8.5 4.2 8.5-4.2"/></svg>`,
	"nav-people":  `<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.9" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><circle cx="9" cy="8" r="3.4"/><path d="M3.2 19.5a5.8 5.8 0 0 1 11.6 0"/><path d="M16.2 5.2a3.4 3.4 0 0 1 0 5.6"/><path d="M18 14.6a5.8 5.8 0 0 1 2.8 4.9"/></svg>`,
	"nav-shield":  `<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.9" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="M12 2.8 20 6v6c0 4.6-3.3 7.9-8 9.2-4.7-1.3-8-4.6-8-9.2V6z"/><path d="m8.8 12 2.2 2.2 4.2-4.4"/></svg>`,
	"nav-access":  `<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.9" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><circle cx="8" cy="12" r="4.5"/><path d="M12.5 12H22"/><path d="M18 12v3.5"/><path d="M21 12v2.5"/></svg>`,

	"nav-docs": `<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.9" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="M4 4.5A1.5 1.5 0 0 1 5.5 3H10a3 3 0 0 1 2 5.2V21a3 3 0 0 0-2-.8H5.5A1.5 1.5 0 0 1 4 18.7z"/><path d="M20 4.5A1.5 1.5 0 0 0 18.5 3H14a3 3 0 0 0-2 5.2V21a3 3 0 0 1 2-.8h4.5a1.5 1.5 0 0 0 1.5-1.5z"/></svg>`,

	"nav-signout": `<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.9" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="M9 21H5a2 2 0 0 1-2-2V5a2 2 0 0 1 2-2h4"/><path d="m16 17 5-5-5-5"/><path d="M21 12H9"/></svg>`,

	// A question mark, for the inline help each section carries.
	"help": `<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><circle cx="12" cy="12" r="9.2"/><path d="M9.2 9.2a3 3 0 0 1 5.8 1c0 2-3 3-3 3"/><path d="M12 17h.01"/></svg>`,

	// An arrow leaving the box: every external link carries it, so nobody
	// follows one expecting to stay inside the console.
	"external": `<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="M14 4h6v6"/><path d="M20 4 11 13"/><path d="M18 14v5a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2V8a2 2 0 0 1 2-2h5"/></svg>`,

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
