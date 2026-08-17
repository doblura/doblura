#!/usr/bin/env python3
"""Check that the chart's webhook wiring agrees with the operator's.

An admission webhook is a set of strings that have to match across three files: the
markers in internal/webhook (which produce config/webhook/manifests.yaml), the
chart's webhook configuration, and the flags the chart passes the manager. Every
mismatch has the same symptom — the API server calls something that is not there,
failurePolicy Fail turns that into a refusal, and the refused person sees a TLS or
404 error with nothing in it about what is wrong.

None of it is caught by `helm lint`, and none of it is visible to a Go test. So it
is checked here, by parsing what Helm actually renders rather than by grepping the
templates: a `grep` for "failurePolicy: Fail" passes just as happily when the line
sits inside a block that is never rendered.
"""
import pathlib
import subprocess
import sys

import yaml

CHART = "charts/doblura"
GENERATED = pathlib.Path("config/webhook/manifests.yaml")

fails = []


def render(*sets):
    args = ["helm", "template", "t", CHART]
    for s in sets:
        args += ["--set", s]
    out = subprocess.run(args, capture_output=True, text=True, check=False)
    if out.returncode != 0:
        sys.exit(f"helm template failed: {out.stderr}")
    return [d for d in yaml.safe_load_all(out.stdout) if d]


def by_kind(docs, kind):
    return [d for d in docs if d.get("kind") == kind]


def check(ok, message):
    print(f"  {'ok  ' if ok else 'FAIL'}  {message}")
    if not ok:
        fails.append(message)


docs = render()
generated = {d["kind"]: d for d in yaml.safe_load_all(GENERATED.read_text()) if d}

# ── 1. The rules come from the markers, not from somebody's memory ──
#
# What is intercepted, on which verbs, with which failure policy: all of it is
# generated. This compares the rendered chart against the generated manifest field
# by field, which is the only way to know `make chart-sync` was run.
for kind in ("MutatingWebhookConfiguration", "ValidatingWebhookConfiguration"):
    rendered = by_kind(docs, kind)
    if len(rendered) != 1:
        check(False, f"exactly one {kind} is rendered (got {len(rendered)})")
        continue
    want = generated[kind]["webhooks"]
    got = rendered[0]["webhooks"]
    check(len(got) == len(want), f"{kind}: {len(want)} webhook(s) rendered")
    for w, g in zip(want, got):
        for field in ("name", "failurePolicy", "matchPolicy", "sideEffects",
                      "timeoutSeconds", "admissionReviewVersions", "rules"):
            check(g.get(field) == w.get(field),
                  f"{kind}: {w['name']}.{field} matches the generated manifest")
        check(g["clientConfig"]["service"]["path"] == w["clientConfig"]["service"]["path"],
              f"{kind}: {w['name']} is served on the path the Go code registers")

# ── 2. failurePolicy is Fail, stated separately because it is a decision ──
#
# The comparison above would pass just as well if both said Ignore. This is the
# invariant, and if somebody changes it deliberately they get to change it here too.
for kind in ("MutatingWebhookConfiguration", "ValidatingWebhookConfiguration"):
    for w in by_kind(docs, kind)[0]["webhooks"]:
        check(w["failurePolicy"] == "Fail",
              f"{w['name']}: failurePolicy is Fail, so a webhook that is down cannot become a silent bypass")

# ── 3. No caBundle in the template, and no cert-manager anywhere ──
#
# The CA is issued by the manager and published by a controller. A caBundle in the
# template would mean a second source of truth, and a cert-manager annotation would
# mean a dependency the project decided against.
for kind in ("MutatingWebhookConfiguration", "ValidatingWebhookConfiguration"):
    cfg = by_kind(docs, kind)[0]
    check(all("caBundle" not in w["clientConfig"] for w in cfg["webhooks"]),
          f"{kind}: no caBundle in the chart — the manager publishes it")
    check(not any("cert-manager" in k for k in (cfg["metadata"].get("annotations") or {})),
          f"{kind}: no cert-manager annotation — the chart does not depend on it")

rendered_text = "\n".join(yaml.dump(d) for d in docs)
check("cert-manager.io" not in rendered_text,
      "nothing in the rendered chart references cert-manager")

# ── 4. The Service, the container port and the flags all agree ──
service = [s for s in by_kind(docs, "Service") if s["metadata"]["name"].endswith("-webhook")]
deployment = by_kind(docs, "Deployment")[0]
container = deployment["spec"]["template"]["spec"]["containers"][0]
args = container["args"]
ports = {p["name"]: p["containerPort"] for p in container["ports"]}

if len(service) != 1:
    check(False, "exactly one webhook Service is rendered")
else:
    svc = service[0]
    port = svc["spec"]["ports"][0]
    check(port["port"] == 443, "the webhook Service publishes 443, which is what the API server dials")
    check(port["targetPort"] == "webhook",
          "the Service targets the container port by NAME, so changing webhook.port cannot point it at a closed port")
    check(svc["spec"]["selector"] == deployment["spec"]["selector"]["matchLabels"],
          "the Service selects the manager's pods")
    for kind in ("MutatingWebhookConfiguration", "ValidatingWebhookConfiguration"):
        for w in by_kind(docs, kind)[0]["webhooks"]:
            check(w["clientConfig"]["service"]["name"] == svc["metadata"]["name"],
                  f"{w['name']} points at the Service the chart creates")

check("webhook" in ports, "the manager declares a container port named webhook")
if "webhook" in ports:
    check(f"--webhook-port={ports['webhook']}" in args,
          "the manager is told to serve on the port it publishes")

# The names in the flags have to be the names of the objects, or the caBundle is
# published into nothing and every create is refused.
for kind, flag in (("ValidatingWebhookConfiguration", "--validating-webhook-config"),
                   ("MutatingWebhookConfiguration", "--mutating-webhook-config")):
    name = by_kind(docs, kind)[0]["metadata"]["name"]
    check(f"{flag}={name}" in args, f"{flag} names the {kind} the chart installs")

svc_name = service[0]["metadata"]["name"] if service else ""
check(f"--webhook-service={svc_name}" in args,
      "the certificate is issued for the Service name the chart uses")
check(any(a.startswith("--quota-exempt-users=system:serviceaccount:") for a in args),
      "the operator's own ServiceAccount is exempt from the quota")
check(any(e["name"] == "POD_NAMESPACE" for e in container.get("env", [])),
      "POD_NAMESPACE is set, so the certificate carries the right DNS names")

# ── 5. A fail-closed webhook is never installed with nothing behind it ──
#
# The footgun this guards: replicaCount 0 with the configurations installed means
# every OdooEnvironment create in the cluster is refused by a webhook that nothing
# is serving.
parked = render("replicaCount=0")
check(not by_kind(parked, "ValidatingWebhookConfiguration"),
      "replicaCount 0 installs no webhook configuration: nothing would be serving it")
parked_args = by_kind(parked, "Deployment")[0]["spec"]["template"]["spec"]["containers"][0]["args"]
check("--webhook-port=0" in parked_args, "replicaCount 0 also switches the webhook server off")

off = render("webhook.enabled=false")
check(not by_kind(off, "ValidatingWebhookConfiguration") and not by_kind(off, "MutatingWebhookConfiguration"),
      "webhook.enabled=false installs no webhook configuration")
off_args = by_kind(off, "Deployment")[0]["spec"]["template"]["spec"]["containers"][0]["args"]
check("--webhook-port=0" in off_args, "webhook.enabled=false switches the webhook server off")

print()
if fails:
    print(f"  {len(fails)} webhook wiring problem(s)")
    sys.exit(1)
print("  webhook wiring OK")
