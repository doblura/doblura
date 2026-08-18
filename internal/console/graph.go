// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Antoni Romera

package console

import (
	"fmt"
	"html"
	"html/template"
	"strings"
)

// The map of what is joined to what.
//
// It exists because the question "why can I not delete this database" has an
// answer made of three objects, and the answer is invisible in a list. Odoo
// delivery is unusually relational for infrastructure: a customer has
// environments, an environment runs on a database, a database is filled from a
// snapshot, a snapshot came from production, and a rehearsal is the evidence
// that lets any of it move to the next version.
//
// Drawn as SVG on the server rather than with a graph library, for the same
// reason the rest of the console is server-rendered: the Content-Security-Policy
// has no 'unsafe-inline' and loads no scripts, so a diagram that needs a runtime
// is a diagram that cannot be shown. It also means the map works in a screenshot
// pasted into a ticket, which is where these end up.

type node struct {
	ID      string
	Kind    string // OdooTenant, OdooEnvironment, ...
	Label   string
	Sub     string // one line under the name: phase, version, size
	State   string // ok | working | bad | idle — drives colour AND shape
	Href    string
	Missing bool // referenced by something, but not found
}

type edge struct {
	From, To string
	Label    string
}

type graph struct {
	Nodes []node
	Edges []edge
}

// layout places nodes in columns by kind, which is the only arrangement that
// stays readable without a solver: the relationships here are layered by nature
// (customer, then environments, then what each one is made of), so a layered
// drawing is not a simplification of the truth but a picture of it.
const (
	colW   = 230
	rowH   = 92
	boxW   = 196
	boxH   = 58
	padX   = 24
	padY   = 26
	colGap = colW - boxW
)

var columnOf = map[string]int{
	"OdooTenant":      0,
	"OdooEnvironment": 1,
	"OdooDatabase":    2,
	"OdooSnapshot":    2,
	"OdooRehearsal":   2,
	"RunboatLink":     2,
}

// render draws the graph. Returns safe HTML because it is assembled here and
// every interpolated value is escaped on the way in.
func (g *graph) render() template.HTML {
	if len(g.Nodes) == 0 {
		return ""
	}
	pos := map[string][2]int{}
	perCol := map[int]int{}
	maxCol := 0
	for _, n := range g.Nodes {
		c := columnOf[n.Kind]
		r := perCol[c]
		perCol[c]++
		pos[n.ID] = [2]int{padX + c*colW, padY + r*rowH}
		if c > maxCol {
			maxCol = c
		}
	}
	rows := 0
	for _, v := range perCol {
		if v > rows {
			rows = v
		}
	}
	w := padX*2 + maxCol*colW + boxW
	h := padY*2 + rows*rowH

	var b strings.Builder
	fmt.Fprintf(&b, `<svg class="map" viewBox="0 0 %d %d" role="img" `+
		`aria-label="How this customer's objects are connected" `+
		`preserveAspectRatio="xMinYMin meet">`, w, h)
	b.WriteString(`<defs><marker id="mapArrow" viewBox="0 0 10 10" refX="9" refY="5" ` +
		`markerWidth="6" markerHeight="6" orient="auto-start-reverse">` +
		`<path d="M0,0 L10,5 L0,10 z" fill="currentColor"/></marker></defs>`)

	// Edges first, so boxes sit on top of the lines that reach them.
	for _, e := range g.Edges {
		a, ok1 := pos[e.From]
		z, ok2 := pos[e.To]
		if !ok1 || !ok2 {
			continue
		}
		x1, y1 := a[0]+boxW, a[1]+boxH/2
		x2, y2 := z[0], z[1]+boxH/2
		mid := x1 + colGap/2
		fmt.Fprintf(&b, `<path class="edge" d="M%d,%d H%d V%d H%d" marker-end="url(#mapArrow)"/>`,
			x1, y1, mid, y2, x2-6)
		if e.Label != "" {
			fmt.Fprintf(&b, `<text class="edge-label" x="%d" y="%d" text-anchor="middle">%s</text>`,
				mid, min(y1, y2)+abs(y2-y1)/2-6, html.EscapeString(e.Label))
		}
	}

	for _, n := range g.Nodes {
		p := pos[n.ID]
		cls := "node state-" + n.State
		if n.Missing {
			cls += " missing"
		}
		if n.Href != "" {
			fmt.Fprintf(&b, `<a href="%s">`, html.EscapeString(n.Href))
		}
		fmt.Fprintf(&b, `<g class="%s">`, cls)
		fmt.Fprintf(&b, `<rect x="%d" y="%d" width="%d" height="%d" rx="7"/>`, p[0], p[1], boxW, boxH)
		// The state stripe: colour is never the only carrier — the stripe has a
		// width, the label spells the state out, and the kind is written on the
		// box. A person who cannot distinguish the hues loses nothing.
		fmt.Fprintf(&b, `<rect class="stripe" x="%d" y="%d" width="4" height="%d" rx="2"/>`,
			p[0], p[1], boxH)
		fmt.Fprintf(&b, `<text class="kind" x="%d" y="%d">%s</text>`,
			p[0]+14, p[1]+18, html.EscapeString(shortKind(n.Kind)))
		fmt.Fprintf(&b, `<text class="name" x="%d" y="%d">%s</text>`,
			p[0]+14, p[1]+36, html.EscapeString(trunc(n.Label, 24)))
		if n.Sub != "" {
			fmt.Fprintf(&b, `<text class="sub" x="%d" y="%d">%s</text>`,
				p[0]+14, p[1]+50, html.EscapeString(trunc(n.Sub, 30)))
		}
		b.WriteString(`</g>`)
		if n.Href != "" {
			b.WriteString(`</a>`)
		}
	}
	b.WriteString(`</svg>`)
	return template.HTML(b.String()) //nolint:gosec // assembled here; every value escaped above
}

func shortKind(k string) string {
	return strings.TrimPrefix(strings.TrimPrefix(k, "Odoo"), "Runboat")
}

func trunc(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}

func abs(i int) int {
	if i < 0 {
		return -i
	}
	return i
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
