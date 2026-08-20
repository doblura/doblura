# Decisions that shape the product

Eight questions whose answers make a different product, not a different feature.
Written down because the expensive mistake is not choosing wrong — it is choosing
by accident, discovering it a year later, and finding the code has already assumed
something else.

Each one records the answer, what it costs, and what it forecloses. Where an
answer creates tension with another, that is stated rather than smoothed over.

Answered 2026-08-19.

---

## 1. Who doblura is for — **both, with multi-customer as an optional layer**

The code already assumed the integrator: `OdooTenant`, per-customer quotas,
personas like `consultancy`, a namespace per customer.

**The cost of "both" is real and lands on the smaller user.** A company running
one Odoo still has to invent a customer record and write `forTenant` on every
environment, to satisfy a model built for somebody who has forty. That is the
whole tax, and it is payable in one place: a default customer, so that a single
company never types the word again. Anything more than that and "both" quietly
becomes "the integrator, and a worse experience for everyone else".

## 2. Hosting — **self-hosted, plus a control plane that sees only metadata**

With a managed service left open as a possibility.

That possibility is what makes the boundary load-bearing NOW rather than later.
Two properties have to hold from the first line of multi-cluster code, or a
managed service becomes a rewrite instead of a business decision:

- the hub never holds credentials to anybody's cluster (see 4),
- the hub never stores customer data — names, counts and states, never a row of
  anybody's database.

Neither is expensive today. Both are impossible to add afterwards.

## 3. Where the paid line falls — **open, with a paid hosted control plane**

Which means the open-source project must stay **completely usable without it**.
The console keeps working against one cluster, for ever, with no degraded mode
and no nag. The paid thing is the hub, and it is worth paying for because
somebody else runs it — not because the free version was made worse.

**Nothing about security is ever behind that line.** A reversible restore, a WAF,
an allowlist: putting any of those in the paid tier turns the free project into
the insecure version, and that gets noticed and said out loud.

## 4. Multi-cluster — **read-federation now, agents when writes are needed**

Two shapes, in order:

- **Now:** one doblura per cluster, autonomous. The console federates in READ
  only, with a kubeconfig per cluster that can read and nothing else.
- **When writing to remote clusters is needed:** agents that PULL from the hub.

The shape deliberately not taken is the obvious one — a control plane that
writes to remote clusters with stored credentials. It is the version everybody
draws first, and it creates the asset an attacker wants: one place holding write
access to every customer's cluster. It also breaks the property the console rests
on today, which is that it holds no permissions of its own and Kubernetes is the
only authorization.

With agents, the permissions live in the customer's cluster, held by the agent
the customer installed. The hub holds none. That is the same property, kept.

## 5. Where the truth lives — **Kubernetes, with a database as a cache**

Kubernetes objects stay the source of truth: `kubectl` is the API and the audit
log is the record. The hub's database is a cache of what is where, and is allowed
to be stale, wrong, or empty without anything breaking.

The failure being avoided: the moment a database is the truth, doblura can
disagree with the cluster, and that is the class of bug nobody can debug because
both sides look right on their own screen.

## 6. Images — **study what is given, and build from a repository**

Building is the visible thing odoo.sh has and doblura does not: push a branch,
get an environment.

**This collides with 2, and the collision has one resolution.** Building means
seeing the customer's source. A hub that sees only metadata cannot build, so
**builds run in the customer's own cluster** and the hub learns "build 12
succeeded, image sha256:…". If a build ever runs on hardware doblura hosts, the
metadata boundary is gone and decision 2 has silently changed.

## 7. Postgres — **integrate with an operator if present, respect an external one if not**

Not managed by doblura. CloudNativePG already does this better than we would, and
point-in-time recovery — the thing genuinely missed on the worst day — is exactly
what it provides. Doblura detects it and uses it; with a plain external Postgres,
nothing changes from today.

## 8. Odoo edition — **Community only**

Enterprise means subscription keys, a private repository, and the possibility
that a test environment touches a real customer's subscription.

That last one is not hypothetical: it is why the restore uses `--copy` rather
than `--move` when a copy lands in a different environment. Odoo records a uuid
per database and identifies itself to its own services with it, so two databases
claiming to be the same installation is already a problem in Community. With
Enterprise it stops being an inconvenience and becomes a duplicated subscription.

## 9. Regulated data — **classify it, refuse what follows, collect the evidence**

Added the same day, and it belongs here because "make certifications easy" is a
question about what the product IS, not a feature request.

**Doblura cannot make anybody compliant with anything**, and a field that implied
otherwise would be worse than no field: whether PCI DSS applies, and which of its
controls, depends on how cards are taken, by whom, and what the acquirer says.
Doblura knows none of that.

What it can do are the two things that are otherwise expensive:

- **the dangerous configuration refuses itself.** A customer record says what its
  production data holds — personal data, cardholder data, special category — and a
  short list of refusals follows, enforced by the API server rather than described
  in a policy document. Short on purpose: only the ones that are wrong whichever
  version of whichever standard applies. A "compliance mode" switching on twenty
  settings would be claiming knowledge of an audit doblura has not read.
- **the evidence is already collected.** What an environment holds is a LABEL, so
  "show me everything with cardholder data in it" is a selector rather than an
  investigation, and it is recorded at creation rather than inferred later from a
  customer record that may have changed since.

The refusals, and why each one is certain:

| what is declared | what is refused | why it is not a judgement call |
| --- | --- | --- |
| cardholder data | `data.type: Live` outside Production, **with no acknowledgement** | scope: the copy puts its environment, its cluster and its backups inside the audit. Refusing costs less than auditing |
| cardholder data | outgoing mail outside Production | a non-production environment that can send is one that can send to real cardholders from a machine nobody watches |
| personal data | turning off `noIndex` | indexed by a search engine is a disclosure nobody had to attack anything to get, and it is reportable |

What is deliberately NOT here: retention and erasure. A right-to-erasure request
against twelve backups of one database is a real problem doblura can see and
cannot solve, and pretending otherwise would be the worst thing in this file.

## 10. Language — **English, except the one screen a customer reads**

Decided 2026-08-19, after the interface pass.

The operator console is in English and stays there. The people who run a platform
share one vocabulary, and doubling several hundred sentences of carefully-worded
explanation would double what has to be kept true: every new message written
twice, every correction applied twice, and one of the two quietly drifting until
the two languages disagree about what a control does.

The status page is different, and it is the only one that is. Its reader is
somebody at the customer's company who was sent a link, who does not work in
Kubernetes, and who is trying to find out whether their Odoo is up before phoning
anybody. That is the one place the language is a barrier rather than a preference.

So the mechanism is general and the translation is not. The language comes from
the CUSTOMER record rather than from the reader's browser or a picker, because the
reader is opening a link they were sent, and asking them to choose a language
before they can find out whether their Odoo is working is one screen too many —
their integrator already knows what language they speak.

Three tests keep it honest, and the second found a real gap the day it was
written: every string exists in every language; every state the health check can
produce has words for a customer (`gone` had none, and fell through to "we cannot
tell", which is wrong in the one direction that matters — the answer IS known, and
it is that somebody switched it off); and every id the template asks for exists.

Adding a third language is a catalogue entry per string and nothing else. Adding
one with real plural classes is the moment to reach for a plural-rule library, and
the comment on `duration` says so.

## 11. A base image — **yes, and it ships tools and defaults, never opinions**

Doblura consumes images and studies what is in them, and the study exists because
this went wrong repeatedly: the official image runs as uid 100 and Doodba's uid 100
is `messagebus`; Doodba's published base ships the scaffolding and not Odoo;
Doodba's command is `python3`, so `-c odoo.conf` hands the config to the
interpreter; and an image without `click-odoo-contrib` cannot restore, update or
back anything up — which is discovered on the day somebody needs a restore.

So doblura publishes a base image per Odoo major. It contains:

- Odoo, at a pinned version, running as a user doblura knows,
- `click-odoo-contrib`, because restore, update and backup all need it,
- the wkhtmltopdf build Odoo actually wants, which is the oldest unpatched
  paper-cut in this ecosystem,
- an entrypoint and a config that behave the way this operator expects.

**And no functional modules.** Not a "performance pack", not a curated OCA
selection, not queue_job. The line is that the image supplies the RUNTIME and
doblura supplies the CONFIGURATION — workers, cron threads, the split between web
and scheduled jobs, the connection proxy — all of which it already sets from
`size` and `workload` and can change without anybody rebuilding anything.

The reason is not purity. A module baked into a base image changes what the ERP
does, in a layer the customer did not choose and cannot see, and it is one that
somebody's auditor will eventually ask about. It is also a maintenance trap: every
bundled module is a version to track against three Odoo majors, and the day one of
them breaks on 19 the base image is stuck. Modules belong in
`spec.addons.repos`, where they are named, pinned to a commit, and visible in the
interface.

What this unblocks is decision 6: building from a repository needs a base to layer
onto, and "whatever image you already have" is not one.

Two things to be deliberate about before publishing anything:

- **Trademark.** "Odoo" is theirs. The image is named for doblura and describes
  what it contains; it does not present itself as an official Odoo build.
- **Enterprise.** Community only, per decision 8. An image that could be mistaken
  for carrying Enterprise code is worse than no image.

## 12. When the hosted control plane happens — **on evidence, not on a date**

Decision 2 left a managed service open and decision 3 makes the hub the paid thing.
The question left is when, and the honest answer is: not until people are running
the self-hosted thing.

The gate is **five installations doblura did not install**, each with a real
customer's environments in it, running for a month without the author touching
them. Not five downloads and not five stars: five clusters where somebody else
chose this, and where the operator survived a month of their reality.

Building the hub before that is building the paid half of a product nobody has
finished evaluating the free half of — and the hub is the piece that cannot be
tested by its author, because its whole value is being run by somebody who does not
want to run it themselves.

## 13. What the paid tier includes — **the hub, and never a safety control**

Concretely, and so it can be argued with:

| Free, for ever | Paid |
| --- | --- |
| The operator, every kind, every guardrail | A hosted hub somebody else runs |
| The console, against as many clusters as you like | Cross-cluster **writes**, via agents |
| Every safety control: reversible restores, WAF, allowlists, anonymisation, the data rules | Inventory and history across clusters, kept for you |
| Single sign-on, every persona | Support with an answer time attached |

The rule that decides anything not on the list: **if a feature makes a failure less
likely or less expensive, it is free.** The moment somebody can say "the paid
version is the safe one", the free project has become the unsafe one, and that is
said out loud once and remembered for ever.

Cross-cluster writes are paid not because they are safer — they are not, they are
the riskier shape — but because they are the thing that only makes sense when
somebody else is operating the hub.

## 14. What "beta" means — **four things, all checkable**

`v0.1, alpha. The API can still change` is the sentence that stops anybody
evaluating this seriously, and it stays true until these are true:

1. **The API is frozen for v1alpha1**, meaning: no field removed or renamed
   without a conversion. Adding is fine; a rename is not.
2. **An upgrade path that is tested**, not asserted: a cluster installed at the
   previous release, upgraded, with its environments still running afterwards —
   in CI, on every release.
3. **Somebody else's cluster.** At least one installation this project did not
   perform, that survived a month. Same evidence as decision 12 asks for, at a
   lower bar.
4. **The destructive paths have been exercised by somebody else.** A restore, a
   major upgrade and a rehearsal, run by a person who did not write them, on data
   they cared about.

Three and four are not code, and that is the point: alpha is not a code quality
statement, it is a statement about how much is known — and nothing here is known
until somebody who is not the author has done it.

## 15. Odoo versions — **the three Odoo supports, and one year of grace**

Doblura supports the three majors Odoo itself supports, plus the one that just
fell out, for a year.

The grace year exists because doblura's whole reason to exist is rehearsing the
migration OFF an old version. Dropping support for 17 the week Odoo does would
remove the tool from exactly the people who most need it — the ones still on 17,
who are the ones with a migration to rehearse. A migration tool that only supports
supported versions has misunderstood its job.

What "support" means here, so it is not a feeling: the guardrails run against it,
a base image exists for it, and a rehearsal from it to the next major is part of
the release check.

## 16. Governance — **one maintainer, said plainly, with the exits marked**

There is one maintainer. Pretending otherwise in a GOVERNANCE.md would be the kind
of thing somebody discovers at the worst moment, so it says one, and it says what
follows:

- **Contributions**: bug fixes and guardrails, gladly. A new CRD or a change to
  the personas is a design conversation before it is a pull request — not
  gatekeeping, but because those two are the API and the security model.
- **The bus factor is one**, written down rather than implied. What protects
  somebody depending on this is not a promise about the maintainer: it is AGPL,
  a repository they can fork, an operator whose state lives entirely in their own
  cluster's objects, and no hosted dependency in the free tier. That is the honest
  answer, and it is a better one than a second name on a file.
- **Anything security-related** gets a fix before it gets a discussion.

---

## 17. Incoming mail — **no, and the copies are stopped from doing it by accident**

Outgoing mail is a field: doblura writes an `ir_mail_server` pointing at a server
you already have, and runs no mail infrastructure. Incoming is not the mirror of
that, and the difference is the whole decision.

Odoo takes mail in two ways. A gateway — an MTA delivering to `odoo-mailgate`,
routed by alias domain — needs infrastructure doblura would then own, which is
the same line decision 2 draws everywhere else. And `fetchmail`, where Odoo polls
an IMAP box, which needs no infrastructure at all and is therefore the tempting
one to support.

It is refused because of what it does when it is pointed at the wrong database.
**Polling consumes.** fetchmail marks messages seen or deletes them, so a copy of
production reading the customer's real mailbox takes mail that was meant for the
real system and files it somewhere nobody is watching. Outgoing mail from a copy
is bad and at least leaves a trace: a person received something they should not
have, and they will say so. Mail that was quietly consumed leaves nothing —
no bounce, no complaint, just an inbox that is emptier than it should be, and
records that exist in the copy and not in the system of record.

So doblura writes no fetchmail server, and there is no field for one. Somebody
who wants incoming mail configures it in Odoo, on the machine they mean.

What doblura does instead is the part that matters: it makes sure a copy cannot
inherit it. `fetchmail_server` is deactivated in the snapshot pipeline and in
every environment's hardening step, guarded on the table existing because
fetchmail is a module. The second of those was missing — the hardening step cut
outgoing mail and crons and left incoming alone, while the snapshot pipeline's
identical list cut all three. Nothing pointed at the asymmetry, which is the
argument for writing this decision down rather than leaving it as an absence.

**Why:** an absent feature that nothing protects against is not a decision, it is
a gap. The decision is the refusal *plus* the neutralization, and only the second
one is code.

---

## What this ordering means for the next work

Done: the default customer (1), read-federation in the console (4, first half),
and the data rules (9). The hub's cache (5) was **measured** rather than argued —
forty customers and 211 environments, every page under a second, including the
view that asks two clusters. It is not needed, and that is now a fact rather than
a guess.

Next, in order:

1. **A base image** (11). It is the piece decision 6 needs, and the one that
   removes the whole class of problem the image study exists to report.
2. **Building from a repository** (6), on top of it, with builds in the
   customer's own cluster.
3. **`OdooRelease`**: rolling one release across many customers. The customer type
   already refers to it in a comment, and the type does not exist — which is the
   shape of defect this project has spent a day removing everywhere else.
4. **Prometheus metrics**: migration duration per release.

Agents (4, second half) come when writes to remote clusters are needed, and the
hosted hub (12) when five other people are running this. Neither is a design
question any more.
