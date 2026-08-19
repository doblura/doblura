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

---

## What this ordering means for the next work

1. A default customer, so decision 1's cost is paid once (small).
2. Read-federation in the console (decision 4, first half).
3. The hub's cache, only when federation needs one (decision 5).

Agents, builds and the hosted control plane come after, and each is a separate
decision about *when*, not about *what* — those are settled above.
