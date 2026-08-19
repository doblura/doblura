# Licensing and governance

Two decisions were made before the first commit, because both get far more
expensive afterwards. They are written down here so nobody has to reconstruct
them from the repository later.

## This repository is AGPL-3.0, permanently

Everything here — the operator, the CRDs, the chart, the tooling — is
[AGPL-3.0-or-later](LICENSE).

The model is Odoo's, deliberately: **a copyleft community edition, and a
proprietary enterprise edition beside it.** Odoo Community is LGPL-3 and Odoo
Enterprise is a proprietary subscription; the same shape applies here, with
AGPL-3 instead of LGPL because AGPL is what the OCA modules this operator exists
to deliver already use.

One correction worth making explicitly, because the assumption is common: **AGPL
does not forbid selling this.** No open-source licence can — free redistribution
is the first clause of the Open Source Definition, which is why Odoo partners sell
Community-based hosting and services perfectly legally. What AGPL adds is
§13: offer it to users over a network and you must offer them the source of your
modified version too.

That is as close as an actual open-source licence gets to "do not resell my work
as a closed service", and it is aimed squarely at a cloud provider wrapping this
in a control plane, not at you running it on your own cluster. Running it
internally, modifying it internally, and never distributing it triggers nothing.

**The cost, stated plainly:** some companies ban AGPL by policy regardless of how
they use it. That is real, it will cost some adoption, and it was accepted in
exchange for the protection above.

### One exception: `api/` is Apache-2.0

[`api/`](api/LICENSE) is Apache-2.0, and that is load-bearing rather than untidy.

`api/v1alpha1` holds the Go types for the CRDs. It is the **integration surface**:
anybody writing Go against Doblura imports it, and Go links statically, so under
AGPL every one of those integrations would be forced to AGPL too — third-party
tooling, internal glue, and the enterprise edition alike. That is friction with no
upside; the value of this project is in the controllers, not in a struct
definition.

So the split follows the dependency direction, which was already clean:

```
api/          Apache-2.0    types only, imports nothing else here
internal/     AGPL-3.0      the controllers — where the work happens
cmd/          AGPL-3.0
charts/       AGPL-3.0
```

The boundary holds only while `api/` depends on nothing under AGPL, and breaking
it would be silent: the code still compiles and every test still passes. So it is
a build check, not a promise — `make verify-licence`, wired into `make all`.

## Contributions: DCO, not a CLA

Sign your commits off:

```bash
git commit -s     # adds: Signed-off-by: Your Name <your@email>
```

That is the whole process. No CLA, no form, no bot — the same arrangement the
Linux kernel and Kubernetes use. [`DCO`](DCO) is the text you are certifying.

**What this costs:** copyright stays distributed across everyone who contributes,
so **this repository can never be relicensed** — not by anybody, its author
included. That would need permission from every contributor.

The limitation is the intended one. It makes the promise above structural rather
than a matter of trusting me: there is no mechanism by which this code later
becomes proprietary, because there is nobody who could grant it.

## Where the commercial part lives, and why it is elsewhere

The paid edition lives in a **separate private repository** under a proprietary
subscription licence — the same arrangement as Odoo Enterprise, not a
source-available scheme with a timer on it.

Keeping the two apart is what makes the DCO choice safe. One repository mixing
community contributions with code bound for a commercial edition is exactly the
situation that forces a CLA, and the CLA is what drives contributors off. Two
repositories, and the question never arises.

| | this repository | the enterprise repository |
| --- | --- | --- |
| Licence | AGPL-3.0 (`api/` Apache-2.0) | proprietary, subscription |
| Source | public | available to subscribers |
| Contributions | anyone, under DCO | not accepted |
| Production use | unrestricted, forever | licensed |
| Relicensing | impossible by construction | n/a |

**The line between them is a commitment, not a marketing boundary.** Everything
needed to rehearse a migration safely on your own cluster is here and stays here:
the rehearsal, the anonymized snapshots, every guardrail, the environments, the
RBAC profiles, the Runboat mirror, the chart. None of it is moved out or crippled
later to create a reason to pay.

What goes on the other side is what only makes sense when somebody else operates
it for you, or at a scale a single team does not have:

- the hosted control plane and the multi-cluster console
- identity beyond OIDC group mapping — SCIM provisioning, audit export
- long-term retention and cross-region snapshot custody
- support with a response time attached

If you can run it yourself, you can run all of it. Any future feature has to pass
that test before it is allowed on the private side.

### How the enterprise side is allowed to talk to this one

Two routes, both legitimate, and the distinction matters:

1. **The Kubernetes API.** Read and write `OdooRehearsal`, `OdooEnvironment`,
   `RunboatLink` as objects, over HTTP, with RBAC deciding. Using an API is not
   linking, so nothing propagates. This is the default and it is what a console
   does anyway.
2. **Importing `api/`.** Legal because that package is Apache-2.0 — which is the
   whole reason for the exception above.

What it may **not** do is import `internal/`. That would make the enterprise
binary a derivative work of AGPL code, and the AGPL would then require its source.
No accident there is possible in this direction either: `internal/` is a Go
internal package, so the compiler refuses the import outright.

## The repositories

Four, and the split follows the commitment above rather than the other way round.

| Repository | Visibility | Licence | What |
| --- | --- | --- | --- |
| `doblura/doblura` | public | AGPL-3.0 (`api/` Apache-2.0) | the operator, the chart, **and the console** |
| `doblura/doblura.dev` | public | AGPL-3.0 | the site and the documentation for self-hosting |
| `doblura/enterprise` | private | proprietary | the hosted and multi-cluster parts |
| `doblura/.github` | public | — | the organisation profile |

### The console is public, and that is not generosity

It is the obvious thing to hold back, and holding it back would break the project.

The five RBAC profiles exist because support, QA and consultancy are not `kubectl`
users. A self-hosted edition that ships `support` and `qa` ClusterRoles with no
interface for the people they are named after ships a feature nobody can reach —
and it would contradict the commitment three sections up, which names the RBAC
profiles explicitly as staying on this side.

So the single-cluster console goes here: the customer list, the task launcher, the
approval queue, OIDC group mapping, all five profiles. What is on the other side
is what genuinely cannot exist on one cluster you already administer:

- **many clusters at once**, and a control plane somebody else operates
- **identity past group mapping** — SCIM provisioning, audit export
- **retention and custody** across regions, over years
- **support** with a response time attached

The test for any future feature: *can a competent team do this on their own
cluster?* If yes, it belongs in the public repository, no matter how sellable it
looks.

### Why the docs are their own repository

Documentation for self-hosting changes on a different clock from the operator: a
typo fix should not need a Go review, and a first contribution to a project is far
more often a docs fix than a controller change. Keeping it separate also keeps it
honest — documentation that lives beside the code it documents drifts more slowly,
but documentation nobody can find is worse, and a site is the findable form.

## Trademark

"Doblura" is a coined name, chosen partly because it is defensible — a common
descriptive word would not have been, which is the reason the project is not
called "rehearsal" or "ensayo".

The code is AGPL-3.0 and **the name is not part of that grant.** Fork it freely;
ship the fork under a different name. That is the ordinary arrangement and the
same one Odoo, Grafana and Kubernetes all use.

## Who maintains this, and what happens if they stop

**One person.** Written here rather than implied, because the alternative is that
somebody discovers it at the worst possible moment.

That is a real risk and the honest thing is to name what actually protects
somebody who depends on this — which is not a promise about the maintainer:

- **AGPL-3.0 and a public repository.** The code cannot be withdrawn. A fork needs
  a new name (see Trademark) and nothing else.
- **The state lives in your cluster.** Every object doblura manages is a Kubernetes
  resource in your own API server. There is no database of ours holding your
  inventory, no account, and nothing to export. If this project stopped tomorrow,
  `kubectl get odooenvironments` keeps working, and so does everything already
  running — the operator reconciles, and an operator that is not running leaves
  workloads alone rather than removing them.
- **No hosted dependency in the free tier.** The console talks to your API server;
  the operator talks to your API server. Nothing calls home, so nothing stops
  working when somebody stops paying a bill.

A second name on this file would be a nicer thing to read and a worse guarantee.

### What a contribution looks like

- **Bug fixes and guardrails**: open a pull request. Guardrails especially — a
  check that catches something this project got wrong is the most useful thing
  anybody can send.
- **A new CRD, or a change to the personas**: a conversation first. Not
  gatekeeping. Those two are the API and the security model, and both are far
  cheaper to argue about before the code than after it.
- **Anything security-related**: it gets a fix before it gets a discussion. If
  it is sensitive, say so and it will be handled privately.

Contributions are under the DCO, as above.
