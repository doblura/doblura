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

print()
if fails:
    print(f"  {len(fails)} rendering problem(s)")
    sys.exit(1)
print("  the rendered chart is sound")
