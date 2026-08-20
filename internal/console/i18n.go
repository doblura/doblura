// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Antoni Romera

package console

import (
	"strconv"
	"time"
)

// Two languages, and only where the language matters.
//
// The operator console is in English and stays there: the people who run a
// platform share one vocabulary, and doubling several hundred sentences of
// carefully-worded explanation would double what has to be kept true — every new
// message written twice, every correction applied twice, and one of the two
// quietly drifting.
//
// The status page is different. Its reader is somebody at the customer's company
// who was sent a link, who does not work in Kubernetes, and who is trying to find
// out whether their Odoo is up before they phone somebody. Making that person read
// English is the one place the language is a real barrier rather than a
// preference.
//
// So: a catalogue, a locale that comes from the CUSTOMER rather than from the
// reader, and English for anything a catalogue does not have. The mechanism is
// here for the rest of the console when somebody wants it; the text is not.

// locale is a two-letter language code. Empty means English.
type locale string

const (
	localeEN locale = "en"
	localeES locale = "es"
)

// catalogue is every string the status page can say.
//
// Keyed by an ID rather than by the English text. Keying by the English would mean
// that improving a sentence silently drops its translation — which is the failure
// this project keeps finding in other shapes: a change in one place that another
// place is supposed to follow, and nothing that notices when it does not.
var catalogue = map[string]map[locale]string{
	"title": {
		localeEN: "Is it working?",
		localeES: "¿Está funcionando?",
	},
	"looked-at": {
		localeEN: "Looked at %s. This page checks again every minute on its own — or",
		localeES: "Consultado a las %s. Esta página vuelve a mirar sola cada minuto — o",
	},
	"look-now": {
		localeEN: "look now",
		localeES: "míralo ahora",
	},
	// The purposes, on the one page written in the customer's language.
	//
	// They are enum values from the CRD and they were printed raw, so a Spanish
	// sentence read "prod · Production · lleva así 2 horas". A page that is
	// translated except for the nouns is a page that looks half-translated, which
	// is the impression it must not give: it is the only screen a customer sees.
	"purpose-Production": {
		localeEN: "Production",
		localeES: "Producción",
	},
	"purpose-Staging": {
		localeEN: "Staging",
		localeES: "Preproducción",
	},
	"purpose-QA": {
		localeEN: "QA",
		// Left as QA: it is what the people who use it call it in Spanish too,
		// and "control de calidad" in a status line reads as a different thing.
		localeES: "QA",
	},
	"purpose-Review": {
		localeEN: "Review",
		localeES: "Revisión",
	},
	"cannot-tell": {
		localeEN: "We cannot tell you right now",
		localeES: "Ahora mismo no podemos decírtelo",
	},
	"cannot-tell-detail": {
		localeEN: "This page could not read the status. Send whoever looks after your " +
			"Odoo exactly this and they will know what it means:",
		localeES: "Esta página no ha podido leer el estado. Manda esto tal cual a quien " +
			"lleva vuestro Odoo y sabrá qué significa:",
	},
	"link-missing-name": {
		localeEN: "The link you were given may be missing your name at the end — a " +
			"status link looks like",
		localeES: "Puede que al enlace que te dieron le falte tu nombre al final — un " +
			"enlace de estado se parece a",
	},
	"nothing-here": {
		localeEN: "There is nothing here to show you",
		localeES: "Aquí no hay nada que enseñarte",
	},
	"nothing-here-detail": {
		localeEN: "Your account can sign in but is not attached to any environment " +
			"yet. Whoever set up your access can attach it.",
		localeES: "Tu cuenta entra, pero todavía no está asociada a ningún entorno. " +
			"Quien te dio el acceso puede asociarla.",
	},
	"all-working": {
		localeEN: "Everything is working",
		localeES: "Todo funciona",
	},
	"one-working": {
		localeEN: "It is working",
		localeES: "Funciona",
	},
	"all-working-detail": {
		localeEN: "If something still looks wrong to you, it is worth reporting — this " +
			"page only knows whether the server is answering, not whether it is " +
			"answering correctly.",
		localeES: "Si aun así ves algo raro, merece la pena avisar: esta página solo " +
			"sabe si el servidor responde, no si responde bien.",
	},
	"your-odoo": {
		localeEN: "Your Odoo",
		localeES: "Vuestro Odoo",
	},
	"since": {
		localeEN: "this has been the case for %s",
		localeES: "lleva así %s",
	},
	"not-measured": {
		localeEN: "Nobody is measuring how hard it is working, so this page cannot " +
			"tell you whether it is under load.",
		localeES: "Nadie está midiendo cuánto trabaja, así que esta página no puede " +
			"decirte si está sobrecargado.",
	},
	"open-it": {
		localeEN: "Open it",
		localeES: "Abrirlo",
	},
	"fineprint": {
		localeEN: "What this page can and cannot tell you: it knows whether the " +
			"server is running and answering, and how much memory it is using " +
			"against what it was given. It does not know whether a report is wrong, " +
			"whether an import finished, or whether something you did yesterday " +
			"saved. For those, tell whoever looks after your Odoo — this page is not " +
			"a substitute for saying something is wrong.",
		localeES: "Lo que esta página puede y no puede decirte: sabe si el servidor " +
			"está en marcha y responde, y cuánta memoria usa de la que tiene " +
			"asignada. No sabe si un informe sale mal, si una importación terminó, " +
			"ni si lo que hiciste ayer se guardó. Para eso, avisa a quien lleva " +
			"vuestro Odoo — esta página no sustituye a decir que algo va mal.",
	},

	// The states, in the words somebody says on the phone.
	"state-up":       {localeEN: "It is working", localeES: "Funciona"},
	"state-degraded": {localeEN: "It is working, but not fully", localeES: "Funciona, pero no del todo"},
	"state-down":     {localeEN: "It is not working", localeES: "No funciona"},
	"state-building": {localeEN: "It is starting up", localeES: "Está arrancando"},
	"state-asleep": {
		localeEN: "It is asleep, and wakes up when somebody opens it",
		localeES: "Está dormido y se despierta cuando alguien lo abre",
	},
	"state-unknown": {localeEN: "We cannot tell right now", localeES: "Ahora mismo no podemos saberlo"},
	// gone is a state environmentHealth produces and nobody had words for: it fell
	// through to "we cannot tell", which is wrong in the one direction that
	// matters — the answer IS known, and it is that somebody switched it off.
	"state-gone": {localeEN: "It has been switched off", localeES: "Está apagado"},

	"detail-up": {localeEN: "It is answering normally.", localeES: "Responde con normalidad."},
	"detail-degraded": {
		localeEN: "Part of it is not running. Whoever looks after your Odoo can see " +
			"the details, and this is the kind of problem worth telling them about " +
			"if you have not already.",
		localeES: "Una parte no está en marcha. Quien lleva vuestro Odoo puede ver el " +
			"detalle, y es de las cosas que conviene contarle si no lo has hecho ya.",
	},
	"detail-down": {
		localeEN: "It is not answering. Whoever looks after your Odoo can see why " +
			"from here — if this is news to you and to them, tell them.",
		localeES: "No responde. Quien lleva vuestro Odoo puede ver por qué desde " +
			"aquí; si es nuevo para ti y para ellos, avísales.",
	},
	"detail-building": {
		localeEN: "It is being set up or restarted. This normally takes a few minutes.",
		localeES: "Se está preparando o reiniciando. Suele tardar unos minutos.",
	},
	"detail-asleep": {
		localeEN: "It was not being used, so it was switched off to save resources. " +
			"Opening it starts it again, which takes about a minute.",
		localeES: "No se estaba usando, así que se apagó para ahorrar recursos. Al " +
			"abrirlo vuelve a arrancar, y tarda alrededor de un minuto.",
	},
	"detail-gone": {
		localeEN: "This environment was shut down on purpose. It is not coming back " +
			"on its own; if you still need it, ask whoever looks after your Odoo.",
		localeES: "Este entorno se apagó a propósito. No va a volver solo; si todavía " +
			"lo necesitas, pídeselo a quien lleva vuestro Odoo.",
	},
	// The load line. Its wording lives in the operator console too, but the
	// customer's screen has to say it in their language or the page is half
	// translated — which is worse than not translated at all, because the reader
	// cannot tell whether the English sentence is a translation nobody did or a
	// detail that only exists in English.
	"load-tight":       {localeEN: "Running out of memory", localeES: "Se está quedando sin memoria"},
	"load-hard":        {localeEN: "Working hard", localeES: "Trabajando mucho"},
	"load-comfortable": {localeEN: "Comfortable", localeES: "Holgado"},
	"load-tight-detail": {
		localeEN: "Using %d%% of what it is allowed. At this level it will be slow, " +
			"and it may be restarted by the cluster.",
		localeES: "Usa el %d%% de lo que tiene asignado. A este nivel irá lento, y el " +
			"cluster puede reiniciarlo.",
	},
	"load-hard-detail": {
		localeEN: "Using %d%% of what it is allowed. Complaints about slowness are " +
			"plausible; below 75%% they usually are not.",
		localeES: "Usa el %d%% de lo que tiene asignado. Quejarse de lentitud es " +
			"razonable; por debajo del 75%% normalmente no lo es.",
	},
	"load-comfortable-detail": {
		localeEN: "Using %d%% of the memory it is allowed. If somebody says it is " +
			"slow, the cause is probably not this environment being short of resources.",
		localeES: "Usa el %d%% de la memoria que tiene asignada. Si alguien dice que " +
			"va lento, la causa probablemente no sea que le falten recursos.",
	},

	// Durations. "6 hours" in the middle of a Spanish sentence is the small
	// wrongness that tells a reader the page was not written for them.
	"minute": {localeEN: "minute", localeES: "minuto"},
	"hour":   {localeEN: "hour", localeES: "hora"},
	"day":    {localeEN: "day", localeES: "día"},

	"detail-unknown": {
		localeEN: "This page could not work out what state it is in, which is itself " +
			"worth reporting.",
		localeES: "Esta página no ha sabido determinar en qué estado está, y eso ya " +
			"merece avisarlo.",
	},
}

// say is the string for this locale, falling back to English.
//
// Named say and not t because the templates register it AS t, and a Go function
// called t collides with every test's *testing.T — which it did, immediately.
//
// A missing ID returns the ID itself rather than an empty string: a screen with a
// gap where a sentence should be looks like a rendering failure, and one showing
// "detail-down" at least says which sentence is missing.
func say(l locale, id string) string {
	entry, ok := catalogue[id]
	if !ok {
		return id
	}
	if s, ok := entry[l]; ok && s != "" {
		return s
	}
	return entry[localeEN]
}

// localeOf is the language a customer's own people read.
//
// From the customer record, not from the reader's browser: the reader is somebody
// at that company opening a link they were sent, and asking them to choose a
// language before they can find out whether their Odoo is up is one screen too
// many. Their integrator already knows what language they speak.
func localeOf(language string) locale {
	switch locale(language) {
	case localeES:
		return localeES
	default:
		return localeEN
	}
}

// duration is "6 horas" or "6 hours", in the reader's language.
//
// Spanish pluralises by adding s to all three of these, which is why this is four
// lines rather than a plural-rule library. A third language with real plural
// classes is the moment to reach for one — and the moment somebody adds it, this
// comment is where they will look.
func duration(l locale, d time.Duration) string {
	switch {
	case d < time.Hour:
		return countOf(l, int(d.Minutes()), "minute")
	case d < 48*time.Hour:
		return countOf(l, int(d.Hours()), "hour")
	default:
		return countOf(l, int(d.Hours()/24), "day")
	}
}

func countOf(l locale, n int, unit string) string {
	word := say(l, unit)
	if n != 1 {
		word += "s"
	}
	return strconv.Itoa(n) + " " + word
}
