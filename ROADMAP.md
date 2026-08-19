# Roadmap

Full plan with diagrams:
<https://claude.ai/code/artifact/e6e27759-324c-4dea-841c-b308c993f2be>

The decisions that shape the product — who it is for, where the paid line falls,
what multi-cluster means — are in [DECISIONS.md](DECISIONS.md). This file is what
gets built; that one is why.

## The pass that comes after the plumbing

The features have been built the way an operator would want them: refusals that
explain themselves, evidence collected, nothing that lies when it does not know.
The next pass is the other half of the same job — the interface.

Three things, and they are not the same thing:

- **Easy to sell.** Somebody who has never seen this has to understand in one
  screen what it does and why it is safer than what they do now. Today the first
  screen assumes you already know what an OdooTenant is.
- **Easy to operate.** The person fielding the phone call should not have to
  learn the object model. Every screen that names a Kubernetes noun to somebody
  who cannot act on it is a screen that failed.
- **Easy to maintain.** For whoever runs it and for whoever changes the code:
  fewer near-identical templates, one vocabulary for state, one place where a
  colour or a word is decided.

And it should be good-looking, which is not decoration — a tool that looks
unfinished is one people assume is unfinished.

## What this is

A personal project, meant to be published. Three goals, turned into verifiable
properties:

- **Secure** — Kubernetes RBAC is the only authorization; no secret ever lives in
  a resource spec; the API server enforces the guardrails, not the docs; no copy
  of production leaves without being neutralized.
- **Simple** — one `helm install` and it works; the defaults hold in production;
  every error says what to do, not just what failed.
- **Scalable** — nothing assumes one instance, one database or one customer;
  placement is a policy; the heavy lifting happens in Jobs, not in the
  controller.

## The unifying idea

The pieces of ERP delivery are the same primitive: materialize an Odoo from
(code × data) and assert something about the result. Two axes vary:

| | data source | lifecycle |
| --- | --- | --- |
| Review environment (PR) | empty or demo | hibernating |
| **Migration rehearsal** | **anonymized snapshot** | **ephemeral** |
| Test environment | anonymized snapshot + subset | ephemeral, exposed |
| Staging | anonymized snapshot | persistent |
| Production | live data | persistent |

Plus a third axis that appears once an environment is exposed: its **security
posture**.

## Topology

Three levels: **Postgres instance → database → Odoo company**.

- Customer ↔ instance is **arbitrary**: a customer spreads databases across
  instances, and an instance hosts databases from several customers. A placement
  policy decides, not a manual assignment.
- Customer ↔ database is **not** arbitrary. Two unrelated customers in the same
  database is the one thing the platform forbids by design. Inside a database
  Odoo shares its master data on purpose, and no filter fixes that.

## Anonymizing and segregating are different

- **Masking** removes the WHO (personal data). Every row is preserved.
- **Subsetting** removes the WHOSE (other companies). Greenmask does it by
  walking the foreign-key graph from `res_company`.

A dump you can hand to a customer needs **both**. Today only the first exists,
which is fine for rehearsing a migration and not for giving to anyone.

## The interface

One, not three. The personas are **ClusterRoles**, not screens: the same customer
list offers different actions depending on who is looking.

A service **separate** from the controller (same chart, like Argo CD), which
authenticates over OIDC and calls the API server **impersonating the user**. That
way Kubernetes RBAC is the single source of truth, a bug in the interface grants
nothing, and the audit log comes for free.

Self-service **with quotas**, enforced by an admission webhook: if support spins
up an environment per ticket, the cluster dies on Friday. That part is **done** and
does not need the interface — the limits are enforced against `kubectl` too, which
is the only way they can be, since the interface has no permissions of its own.

## Phases

| | What | Status |
| --- | --- | --- |
| **0a** | Everything in English: comments and CRD field descriptions | **done** |
| **0b** | A real end-to-end rehearsal against a real Odoo | **done** — Odoo 19, 7 bugs found |
| 1 | `OdooInstance` + `OdooDatabase` + `OdooTenant` + placement | **done** |
| 2 | Finish the `OdooEnvironment` controller + company subsetting | **done** |
| 3 | The interface: customer list, OIDC. The 5 ClusterRoles and the **quota webhook are done** | partly done |
| 4 | `OdooRelease`: customer batches with soak time — canary across customers | pending |
| 4b | Metrics: delivery plus the Odoo runtime signals above | pending |
| 5 | `OdooProject`: consumes a release, adds its own addons | pending |
| 6 | Multi-stage `OdooRehearsal` with OpenUpgrade | pending |
| 7 | Publish: chart on OCI, release, docs site | pending |
| 8 | Submit to **OCA** and **AEODOO**, logo and final name, promotion | pending |

## Metrics

Two kinds, and the second is the interesting one.

### Delivery metrics — what Doblura already knows

It measures these today and throws them away; publishing them is mostly wiring.

| Metric | Why it matters |
| --- | --- |
| `doblura_migration_duration_seconds{tenant,release,stage}` | The number the maintenance window is planned from. Its **trend** is the real signal: a `-u` going from 40 minutes to 3 hours over six months is invisible until the window breaks. |
| `doblura_budget_exceeded_total{tenant}` | How often a release finished cleanly and still did not fit. |
| `doblura_rehearsal_result_total{result}` | Pass/fail rate. A suspiciously perfect rate usually means the rehearsal is not exercising anything. |
| `doblura_snapshot_age_seconds{database}` | A three-month-old anonymized dump makes every rehearsal lie. This is the metric that catches it. |
| `doblura_environments_open{tenant}` | Quota pressure, and the number that predicts a Friday outage. |
| `doblura_instance_databases{instance}` / `_available` | Placement headroom, before it runs out at 3am. |

### Odoo runtime metrics — and why they are also guardrails

This is the part a generic platform cannot do, because it requires knowing what
these tables mean.

| Metric | Source | Reads as |
| --- | --- | --- |
| `odoo_cron_overdue_seconds{job}` | `ir_cron.nextcall` | A cron that has not run since Tuesday is a silent production failure. Nothing alerts on it today. |
| `odoo_cron_active{env}` | `ir_cron.active` | In a **non-production** environment this must be **zero**. |
| `odoo_mail_queue{state}` | `mail_mail.state` | Outgoing depth and failures. In a neutralized environment it must be **zero**. |
| `odoo_queue_job{state}` | `queue_job.state` (OCA) | The production health signal for OCA deployments: pending, started, failed, and the age of the oldest pending job. |
| `odoo_queue_job_retries{channel}` | `queue_job.retry` | A job retrying forever is worse than a job that failed, because nobody notices. |
| `odoo_automation_active{model}` | `base_automation` | Automations firing in a test environment reach outside as surely as a cron does. |
| `odoo_db_size_bytes{database}` | `pg_database_size` | Feeds placement, and predicts the next migration's duration better than anything else. |
| `odoo_filestore_bytes{env}` | filestore volume | The half of a backup people forget until a restore leaves orphaned attachments. |

**The insight worth building around:** the last four in that table are not
observability, they are *verification that a guardrail did not silently fail*.

Neutralization is the most dangerous thing this platform does. `odoo neutralize`
disables crons, outgoing mail servers, payment providers and carriers — and if it
silently did not, the way you find out is a customer receiving a real invoice from
a test environment. But a neutralized environment with `mail_mail` in state
`outgoing`, or `ir_cron.active = true`, or `queue_job` in `started`, is
**provably** un-neutralized. Those metrics turn the scariest failure mode from
"discovered by a customer" into an alert.

So the design follows from that: they are not optional dashboards, they are
assertions with a Prometheus interface. `mail_queue > 0` on anything that is not
production is a page, not a graph.

### Systems metrics — aggregated by tenant, which is the whole point

cAdvisor and kube-state-metrics already publish CPU, memory and volume usage per
pod. None of that answers the question anybody actually asks, which is *per
customer*. The platform's job is the aggregation, and it can do it because every
pod it creates carries `doblura.dev/tenant` and `doblura.dev/environment` labels.

| Metric | Reads as |
| --- | --- |
| `doblura_tenant_cpu_seconds{tenant}` / `_memory_bytes` | What this customer costs right now, across every environment they have open. |
| `doblura_tenant_storage_bytes{tenant,kind}` | Split by kind: filestore, database, dumps, backups. Dumps are usually the surprise. |
| `doblura_instance_connections{instance}` / `_max` | See the connection footgun below. |
| `doblura_instance_disk_free_bytes{instance}` | Feeds `capacity.reservedGi`, and a rehearsal needs its staged copy on top of what it restores. |
| `doblura_pg_longest_transaction_seconds{instance}` | A migration holding locks while production waits is the shape of a bad afternoon. |
| `doblura_pg_cache_hit_ratio{instance}` | The number that explains "the same `-u` took twice as long today". |
| `doblura_image_pull_seconds{image}` | With many short-lived environments, pulling can dominate their whole lifetime. |

**The connection footgun, and it changes the API.** Odoo opens roughly
`workers × (1 + max_cron_threads)` connections per instance. With the defaults
this project generates that is around six per environment — so thirty
environments on one Postgres want 180 connections, and the default
`max_connections` is 100. You hit that long before you run out of disk or hit
`maxDatabases`.

Which means `InstanceCapacity` is currently measuring the wrong thing, or at
least not enough of it: **connections are the binding constraint, not database
count.** The metric comes first, so the limit can be set from evidence rather
than from a guess, and then `capacity` gains a `maxConnections`.

### Usage and quotas — the unit is environment-hours

A shared cluster with self-service becomes a tragedy of the commons in about a
month: support opens an environment per ticket, nobody closes any, and the
pressure is invisible until something is evicted at 3am. TTL is the mechanism
that prevents it. These are what tell you whether the TTL is set right.

| Metric | Reads as |
| --- | --- |
| `doblura_environment_hours_total{tenant,purpose}` | **The unit of account.** What the platform costs, attributable to a customer and to a reason. Without it, cost is a single unsplittable number. |
| `doblura_rehearsal_cpu_hours_total{tenant}` | Rehearsals are the expensive workload: they restore, migrate and discard. Worth knowing separately. |
| `doblura_storage_gib_hours_total{tenant,kind}` | Storage billed by time held, not by peak. |
| `doblura_quota_usage_ratio{tenant}` | How close to `maxEphemeralEnvironments`. Anything sitting at 1.0 is a quota that needs a conversation, not a raise. |
| `doblura_quota_denied_total{tenant,persona}` | Denials from the admission webhook. A rising count means the quota is wrong **or** somebody found a workflow that abuses it — and which of the two is a question the number lets you ask. |
| `doblura_environment_idle_seconds{env}` | Hibernation candidates. An environment nobody has opened in three days is paying rent. |

`environment_hours` split by `purpose` — review, rehearsal, customer preview,
staging — is what turns "the cluster is expensive" into a decision. Usually it
reveals that the expensive thing is not what anyone assumed.

### How, not what

Consistent with the rest: **no home-grown exporter.** These are SQL queries
against a Postgres, which is what `postgres_exporter` custom queries already do
well. Doblura ships them as a preset — the same shape as the masking presets: the
tool is generic and public, the knowledge of *which tables mean what in Odoo* is
the part that does not exist anywhere else.

Caveat to design around from the start: querying `queue_job` and `mail_mail` on a
busy production database costs something. The queries need bounded cost and a
sane scrape interval, or the monitoring becomes the load.

## Integrations

### Runboat

Runboat stays the tool for per-PR review environments — Doblura does not
reimplement it. What Doblura adds is a single pane of glass, so nobody has to
learn two interfaces:

- A `RunboatLink` resource declaring a Runboat instance (base URL, admin
  credentials, which repositories map to which `OdooProject`).
- A controller that polls Runboat's REST API and mirrors its builds as
  **read-only** objects. Runboat remains the source of truth.
- The build actions (start, stop, reset) proxied through, so the same customer
  list that shows environments and releases also shows review builds and can
  drive them.

Federating rather than reimplementing: the mirror is cheap, and the day Runboat
changes its API only the adapter moves.

Phase 0b is a gate. **There are three API types designed and zero real runs**:
the bias towards designing contracts instead of executing code is the only real
risk this project has.

## Non-goals

- **Not another way to run Odoo.** Bitnami's chart and three operators exist.
  This is about the *delivery* lifecycle.
- **Not a replacement for click-odoo-contrib, marabunta, doodba or greenmask.**
  It orchestrates them.
- **No home-grown anonymization.** It configures greenmask, which does it well.
- **Not a Runboat reimplementation.** It remains the best option for per-PR
  environments; with `OdooProject` in place they coexist without overlapping.
