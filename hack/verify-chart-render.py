#!/usr/bin/env python3
"""Check the chart's rendered output for things templates get wrong quietly.

`helm lint` reads templates and `helm template` proves they render. Neither looks
at what came out. These are the mistakes that survive both, found by rendering the
chart and inspecting the objects.

Every check here is a bug this chart actually had.
"""
import collections
import subprocess
import sys

import yaml

CHART = "charts/doblura"
fails = []


def render(*sets):
    args = ["helm", "template", "t", CHART]
    for s in sets:
        args += ["--set", s]
    out = subprocess.run(args, capture_output=True, text=True, check=False)
    if out.returncode != 0:
        sys.exit(f"helm template failed: {out.stderr}")
    return [d for d in yaml.safe_load_all(out.stdout) if d]


def check(cond, msg):
    print(("  ok    " if cond else "  FAIL  ") + msg)
    if not cond:
        fails.append(msg)


def containers(docs):
    for d in docs:
        if d.get("kind") not in ("Deployment", "StatefulSet", "DaemonSet", "Job"):
            continue
        spec = d["spec"]["template"]["spec"]
        for c in spec.get("initContainers", []) + spec.get("containers", []):
            yield d["metadata"]["name"], c


docs = render("console.enabled=true",
              "console.localAccounts.secretName=users",
              "console.sessionKeySecret=session")

# An environment variable defined twice: Kubernetes keeps the last one and warns
# that the earlier may be dropped. The manager had all three of its auxiliary
# image settings emitted twice — once explicitly and once by a range over the same
# values — so which value applied depended on ordering nobody was reading.
for name, c in containers(docs):
    names = [e["name"] for e in c.get("env", [])]
    dupes = sorted(n for n, k in collections.Counter(names).items() if k > 1)
    check(not dupes, f"{name}/{c['name']}: no environment variable is defined twice"
                     + (f" (found {', '.join(dupes)})" if dupes else ""))

# A container that mounts a Secret the chart does not create and the values do not
# name is a pod stuck in ContainerCreating with the reason three kubectl commands
# away.
declared = {d["metadata"]["name"] for d in docs if d.get("kind") == "Secret"}
given = {"users", "session"}
for d in docs:
    if d.get("kind") != "Deployment":
        continue
    for v in d["spec"]["template"]["spec"].get("volumes", []):
        sec = (v.get("secret") or {}).get("secretName")
        if sec and sec not in declared | given and not sec.endswith("-webhook-cert"):
            check(False, f"{d['metadata']['name']} mounts Secret {sec}, which nothing creates or names")

# Every workload the chart ships must be able to run under a restricted Pod
# Security Standard, because that is where anybody sensible installs an operator
# and the failure is a pod that never starts.
for name, c in containers(docs):
    sc = c.get("securityContext", {})
    check(sc.get("allowPrivilegeEscalation") is False
          and sc.get("readOnlyRootFilesystem") is True
          and (sc.get("capabilities") or {}).get("drop") == ["ALL"],
          f"{name}/{c['name']}: runs restricted (no escalation, read-only root, no capabilities)")

# The manager can read every kind it ships a CRD for.
#
# The trap this catches: RBAC markers are package-scoped and controller-gen only
# reads them from a FREE-FLOATING comment. Attached to a declaration's doc comment
# they generate nothing — silently. Everything compiles, every test passes, and
# the manager is denied on its own kind the first time somebody creates one.
# CONTRIBUTING.md documents it, and this was still written the wrong way.
crds = [d for d in docs if d.get("kind") == "CustomResourceDefinition"]
granted = set()
for d in docs:
    if d.get("kind") not in ("ClusterRole", "Role"):
        continue
    for rule in d.get("rules") or []:
        if "doblura.dev" in (rule.get("apiGroups") or []):
            for res in rule.get("resources") or []:
                if {"get", "list", "watch"} <= set(rule.get("verbs") or []) or "*" in (rule.get("verbs") or []):
                    granted.add(res)
missing = sorted(d["spec"]["names"]["plural"] for d in crds
                 if d["spec"]["names"]["plural"] not in granted)
check(not missing,
      "the manager may read every kind the chart installs"
      + (f" (not: {', '.join(missing)})" if missing else ""))

# A NON-EMPTY object default and its fields' defaults have to agree.
#
# An EMPTY one — `default: {}` — is the ordinary case and is fine: defaulting
# recurses, so the object materialises and its fields then get their own defaults.
# Measured rather than assumed, on an environment that declared no security block
# at all: it came back with adminUsers ["admin"] and denyEgress true.
#
# The exception is an object carrying a CEL rule that reads one of its keys. There
# `{}` is refused by the API server itself — "no such key: daily evaluating rule"
# — so the default has to spell the values out, and then the same numbers exist in
# two places. That drift would be silent and would matter: the object default is
# what an unspecified retention gets, the field defaults are what a partly
# specified one gets, and a backup whose retention keeps nothing deletes every
# copy it takes.
for d in docs:
    if d.get("kind") != "CustomResourceDefinition":
        continue
    for version in d["spec"]["versions"]:
        spec = version["schema"]["openAPIV3Schema"]["properties"].get("spec", {})
        for pname, prop in (spec.get("properties") or {}).items():
            obj_default = prop.get("default")
            if not isinstance(obj_default, dict) or not obj_default:
                continue
            if not prop.get("properties"):
                continue
            field_defaults = {k: v["default"] for k, v in prop["properties"].items()
                              if "default" in v}
            if not field_defaults:
                continue
            check(obj_default == field_defaults,
                  f"{d['metadata']['name']}: {pname}'s object default matches its "
                  f"fields' defaults"
                  + ("" if obj_default == field_defaults
                     else f" ({obj_default} vs {field_defaults})"))

print()
if fails:
    print(f"  {len(fails)} rendering problem(s)")
    sys.exit(1)
print("  the rendered chart is sound")
