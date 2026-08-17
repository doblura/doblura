# Contributing

## Sign your commits off

```bash
git commit -s      # adds: Signed-off-by: Your Name <your@email>
```

That is the whole legal process. No CLA, no form, no bot — just the
[DCO](DCO), the same arrangement the Linux kernel and Kubernetes use. It also
means this repository can never be relicensed away from AGPL-3.0, by anyone,
which is the point. See [GOVERNANCE.md](GOVERNANCE.md).

New files get the header:

```go
// SPDX-License-Identifier: AGPL-3.0-or-later
```

## Before opening a PR

```bash
make generate      # never hand-edit generated files: they diverge
make test
make e2e           # creates a kind cluster, installs the CRDs, checks the guardrails
```

## The contract is a public API

`api/v1alpha1` is a contract with people you do not know. Before adding a field:

- **Intent, not configuration.** `size: large`, not `memory: 8Gi`. The
  translation lives in `internal/controller/sizes.go` and can change without
  anyone editing their manifests.
- **Validate in the API server**, not in the reconciler: `Enum`, `default`,
  `Pattern`, `XValidation`. A rejected `apply` is worth more than an event
  twenty seconds later.
- **Pointers for optional fields**, so "unspecified" can be told from zero.
- If you find yourself needing `extraArgs` often, **a field is missing**.
- Field descriptions are **user-facing documentation**: they show up in
  `kubectl explain` and on Artifact Hub. Write them for someone evaluating the
  project, not for yourself.

## Language

Everything is in English: code, comments, field descriptions, docs. This is not
a style preference — a contributor who cannot read the API documentation cannot
contribute.

## The invariants that are not up for negotiation

There are tests defending these. If you change one, the test fails, and that is
working as intended:

1. **`neutralize` defaults to true** and disabling it requires the literal
   acknowledgement. An un-neutralized production dump sends real invoices.
2. **Addons are never copied into a persistent volume.** emptyDir for clones,
   ReadOnly for PVCs, and baked addons are read where they are.
3. **Credentials never go in the URL** nor in `.git/config`, and git output is
   filtered.
4. **The budget is not a timeout.** Exceeding it fails the rehearsal through its
   own condition, separate from `Migrated`.
5. **Truncating tables is opt-in.** The point of anonymizing is to lose sensitive
   data, not rows — and emptying `mail_message` makes the measured duration stop
   predicting the real window.
6. **A `RunboatLink` is read-only until somebody says otherwise.** An empty
   `allowedActions` denies everything, and action requests are idempotency-keyed
   so a reconcile never re-fires a `Reset`.

## Two traps this codebase has already fallen into

Both cost real time. Neither is caught by a unit test, so they are written down:

**A CEL rule on a field that might be absent errors, it does not return false.**
`self.foo` where `foo` was omitted is `no such key`, and an erroring rule rejects
*every* object of that kind — a valid one included. Guard with `has()` first, and
prove it with a guardrail check that expects `ok`, not one that expects
`rejected`. That check is the only thing that distinguishes a working rule from a
CRD nobody can use.

**Grepping is not verifying.** Every masking transformer was missing its closing
quote for the whole life of that function — the config was never valid YAML — and
all the tests passed, because they asserted with `strings.Contains`. If you
generate a config, parse the result in the test, or run the real tool against it
(`make verify-greenmask`).

## Reconciler style

Idempotent, Server-Side Apply with a `FieldOwner`, `SetControllerReference` on
every child, and all three defences against the hot loop at once: compare the
status before writing it, `equality.Semantic.DeepEqual`, and
`GenerationChangedPredicate`.
