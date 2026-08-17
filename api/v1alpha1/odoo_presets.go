// SPDX-License-Identifier: Apache-2.0
// Copyright (C) 2026 Antoni Romera

package v1alpha1

// ────────── Odoo presets: the part you cannot improvise ──────────
//
// Knowing WHICH tables can be emptied and WHICH columns hold personal data in
// an Odoo database is domain knowledge that takes years to accumulate and is
// written down nowhere. These lists are the core value of this type: the generic
// anonymization tool already exists; what does not exist is knowing where to
// point it.
//
// The pg_dump detail that makes this possible
// ───────────────────────────────────────────
// "Odoo will not let you skip tables" is true of its own backup system, but the
// real problem is different: people reach for `pg_dump --exclude-table`, which
// removes the WHOLE table, and then the restore leaves a schema Odoo's ORM does
// not recognise and everything falls over.
//
// What you want is `--exclude-table-data`: it keeps the table, the schema, the
// indexes and the constraints, and omits ONLY the rows. Odoo starts perfectly
// against an empty mail_message.
//
// That single distinction is the whole difference between "impossible" and
// "straightforward".

// TruncatePresetOdoo lists tables that CAN be emptied if you choose to pay the
// price. They are not emptied by default.
//
// The price: on a database with years of history, mail_message and its
// satellites are more than half the size — and also a meaningful share of the
// migration TIME. Emptying them turns a 40 GB dump into 8 GB and a three-hour
// rehearsal into forty minutes, but then those three hours are no longer a fact
// about production, and the maintenance window budget ends up computed against a
// database that does not exist.
//
// When it does make sense:
//   - the copy feeds a development environment where only startup matters
//   - the full dump does not fit the hardware you have
//   - you already know the real duration from another rehearsal and this one
//     measures something else
//
// When it does NOT:
//   - you are going to use the duration to plan a window
//   - somebody will use the environment and expects to see history
var TruncatePresetOdoo = []string{
	// Messaging: the chatter. Suspect number one for size.
	"mail_message",
	"mail_notification",
	"mail_tracking_value",
	"mail_mail",
	"mail_followers",

	// Real-time bus: ephemeral by definition.
	"bus_bus",
	"bus_presence",

	// Logs and audit trails.
	"ir_logging",
	"auditlog_log",          // OCA
	"auditlog_http_session", // OCA
	"auditlog_http_request", // OCA

	// Job queues: re-running old jobs inside a rehearsal is precisely what you
	// do not want.
	"queue_job",          // OCA
	"queue_job_function", // OCA

	// Imports and temporary attachments.
	"base_import_import",
	"base_import_mapping",
}

// TruncateNeverOdoo lists tables that look like candidates and are not. They are
// here to document the trap, and the controller ignores requests to empty them.
//
// The logic: if a table takes part in the schema migration or in bringing up the
// ORM, emptying it turns the rehearsal into a lie. A rehearsal that passes
// against data production does not have is worth nothing.
var TruncateNeverOdoo = map[string]string{
	"ir_module_module":    "module state IS what the migration modifies",
	"ir_model":            "the ORM does not start without the model registry",
	"ir_model_fields":     "same; and it is where the migration adds and drops columns",
	"ir_model_data":       "the XML IDs: without them the migration finds nothing",
	"ir_ui_view":          "views are validated during the -u; empty, nothing is validated",
	"ir_config_parameter": "configuration the migration reads",
	"res_users":           "no users means no login and no permissions to test",
	"res_groups":          "access rights take part in the migration",
	"res_company":         "almost everything hangs off the company",
	"ir_attachment":       "holds the web assets, not just documents: empty it and Odoo cannot serve a page",
	"account_move":        "business data: where migrations fail MOST often",
	"account_move_line":   "same, and it is the largest table that actually matters",
	"stock_move":          "business data",
	"sale_order":          "business data",
	"purchase_order":      "business data",
}

// MaskRulePreset is a masking rule.
type MaskRulePreset struct {
	Table  string
	Column string
	// Kind indicates which generator to use. It is translated to the concrete
	// transformer of the chosen engine in internal/controller.
	Kind MaskKind
	// Note explains why, so the generated YAML can be audited.
	Note string
}

// MaskKind is the class of fake data to generate.
// +kubebuilder:validation:Enum=Name;FirstName;LastName;Email;Phone;Address;City;Zip;VAT;IBAN;URL;Text;Null;Fixed;Hash
type MaskKind string

const (
	MaskName      MaskKind = "Name"
	MaskFirstName MaskKind = "FirstName"
	MaskLastName  MaskKind = "LastName"
	MaskEmail     MaskKind = "Email"
	MaskPhone     MaskKind = "Phone"
	MaskAddress   MaskKind = "Address"
	MaskCity      MaskKind = "City"
	MaskZip       MaskKind = "Zip"
	MaskVAT       MaskKind = "VAT"
	MaskIBAN      MaskKind = "IBAN"
	MaskURL       MaskKind = "URL"
	MaskText      MaskKind = "Text"
	// MaskNull empties the column. For blobs and free-text notes, where
	// anything could be inside and guessing is not worth it.
	MaskNull MaskKind = "Null"
	// MaskFixed sets a constant value.
	MaskFixed MaskKind = "Fixed"
	// MaskHash replaces the value with a hash of the original: it reveals
	// nothing but preserves equality, so joins and duplicates keep behaving the
	// same way.
	MaskHash MaskKind = "Hash"
)

// MaskPresetOdoo lists the columns holding personal data in a standard Odoo.
//
// The criterion: mask what identifies a person, and do NOT touch what the
// migration needs or what makes the environment unusable.
//
// A note on determinism: every one of these rules is applied with the
// deterministic (hash) engine, not the random one. If "Jane Doe" becomes a
// different name in every dump, QA cannot reproduce last week's bug report and
// you cannot compare two rehearsals. Determinism is what keeps an anonymized
// environment usable rather than merely lawful.
var MaskPresetOdoo = []MaskRulePreset{
	// ── Contacts: the bulk of the personal data ──
	{"res_partner", "name", MaskName, "directly identifying"},
	{"res_partner", "email", MaskEmail, "directly identifying"},
	{"res_partner", "email_normalized", MaskEmail, "normalized copy of the above"},
	{"res_partner", "phone", MaskPhone, ""},
	{"res_partner", "mobile", MaskPhone, ""},
	{"res_partner", "street", MaskAddress, ""},
	{"res_partner", "street2", MaskAddress, ""},
	{"res_partner", "city", MaskCity, ""},
	{"res_partner", "zip", MaskZip, ""},
	{"res_partner", "vat", MaskVAT, "tax ID: personal data for sole traders"},
	{"res_partner", "website", MaskURL, ""},
	{"res_partner", "comment", MaskNull, "free-text notes: anything could be in there"},
	{"res_partner", "ref", MaskHash, "hashed rather than nulled: some logic depends on it existing"},

	// ── Chatter: rows PRESERVED, content destroyed ──
	//
	// This is the part that makes emptying mail_message unnecessary. A message
	// body is free text and it can hold literally anything: addresses, phone
	// numbers, bank details pasted into a note. We replace the content and keep
	// the rows, the indexes and the relations.
	//
	// Honest caveat: the replacement text does not have the same length as the
	// original, so the TOAST volume shifts a little and the measured duration
	// deviates slightly from the real one. That deviation is far smaller than
	// the one you get from not having the rows at all, and it is the right
	// trade.
	{"mail_message", "body", MaskText, "free text: the worst place to look for personal data, and the most likely"},
	{"mail_message", "email_from", MaskEmail, ""},
	{"mail_message", "author_avatar", MaskNull, "image: could be a photo of a person"},
	{"mail_message", "subject", MaskText, ""},
	{"mail_tracking_value", "old_value_char", MaskHash, "stores previous field values, personal ones included"},
	{"mail_tracking_value", "new_value_char", MaskHash, ""},
	{"mail_mail", "email_to", MaskEmail, ""},
	{"mail_mail", "email_cc", MaskEmail, ""},
	{"mail_mail", "body_html", MaskText, ""},

	// ── Bank accounts ──
	{"res_partner_bank", "acc_number", MaskIBAN, "financial data"},
	{"res_partner_bank", "acc_holder_name", MaskName, ""},

	// ── Employees: the most sensitive category ──
	{"hr_employee", "private_email", MaskEmail, ""},
	{"hr_employee", "private_phone", MaskPhone, ""},
	{"hr_employee", "private_street", MaskAddress, ""},
	{"hr_employee", "identification_id", MaskHash, "identity document"},
	{"hr_employee", "ssnid", MaskHash, "social security number"},
	{"hr_employee", "passport_id", MaskHash, ""},
	{"hr_employee", "bank_account_id", MaskNull, ""},
	{"hr_employee", "birthday", MaskNull, "personal data, and almost never needed in a rehearsal"},
	{"hr_employee", "km_home_work", MaskNull, "allows inferring the home address"},

	// ── CRM ──
	{"crm_lead", "contact_name", MaskName, ""},
	{"crm_lead", "partner_name", MaskName, ""},
	{"crm_lead", "email_from", MaskEmail, ""},
	{"crm_lead", "phone", MaskPhone, ""},
	{"crm_lead", "mobile", MaskPhone, ""},
	{"crm_lead", "street", MaskAddress, ""},
	{"crm_lead", "city", MaskCity, ""},
	{"crm_lead", "zip", MaskZip, ""},

	// ── Users: CAREFUL, see the comment below ──
	//
	// The login is NOT masked by default, and that is deliberate: change it and
	// nobody can log into the environment to validate it by hand. If your logins
	// are personal email addresses, turn on maskUserLogins and accept that you
	// will need a technical account to get in.
	//
	// The password IS always touched, and not with a random value: with a known
	// one. Random would leave the environment unreachable, which is the same
	// problem by another route.
}

// MaskPresetUserLogins holds the extra rules applied when login masking is
// enabled. Kept separate from the main preset because they break access to the
// environment.
var MaskPresetUserLogins = []MaskRulePreset{
	{"res_users", "login", MaskEmail, "CAREFUL: nobody will be able to log in with their usual account"},
}

// PasswordResetValue is the password assigned to every user.
//
// Constant and known, not random: an anonymized environment nobody can log into
// is useless for validating anything. The risk is bounded elsewhere (the
// environment is not reachable from outside), not by setting a password nobody
// knows.
//
// See OdooEnvironment: for a publicly exposed environment this value is NOT
// acceptable, and randomizeUserPasswords is mandatory there.
const PasswordResetValue = "doblura"
