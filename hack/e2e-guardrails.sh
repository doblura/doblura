#!/usr/bin/env bash
# Check that the API server rejects what it should reject.
# The guardrails live in the CRDs, not in the documentation: this proves it.
set -uo pipefail
fails=0

check() { # name, yaml, ok|rejected
  if printf '%s' "$2" | kubectl apply --dry-run=server -f - >/dev/null 2>&1; then r=ok; else r=rejected; fi
  if [ "$3" = "$r" ]; then printf '  ok    %s\n' "$1"; else printf '  FAIL  %s: %s (expected %s)\n' "$1" "$r" "$3"; fails=$((fails+1)); fi
}

REH='apiVersion: doblura.dev/v1alpha1
kind: OdooRehearsal
metadata: {name: guardrail-check}
spec:
  image: img
  database: {host: h, user: u, passwordSecret: s}
'
ENV='apiVersion: doblura.dev/v1alpha1
kind: OdooEnvironment
metadata: {name: guardrail-check}
spec:
  image: img
  database: {host: h, user: u, passwordSecret: s}
'
SNAP='apiVersion: doblura.dev/v1alpha1
kind: OdooSnapshot
metadata: {name: guardrail-check}
spec:
  image: img
  work: {host: w, user: u, passwordSecret: s}
  to: {type: Volume, volume: {claimName: d}}
'
VOL='  snapshot: {from: {type: Volume, volume: {claimName: d}}}'
ACK_MAIL=i-accept-this-can-send-real-emails-and-charge-real-cards
ACK_PROD=i-accept-reading-from-production-and-the-load-it-causes
ACK_REID=i-accept-anonymized-data-can-still-be-reidentified

echo "-- neutralization --"
check "neutralize:false without acknowledgement" "$REH  snapshot: {from: {type: Volume, volume: {claimName: d}}, neutralize: false}" rejected
check "neutralize:false with a wrong value"      "$REH  snapshot: {from: {type: Volume, volume: {claimName: d}}, neutralize: false, unsafeAcknowledgement: sure-whatever}" rejected
check "neutralize:false with the literal value"  "$REH  snapshot: {from: {type: Volume, volume: {claimName: d}}, neutralize: false, unsafeAcknowledgement: $ACK_MAIL}" ok

echo "-- snapshot provider union --"
check "type Volume without volume"      "$REH  snapshot: {from: {type: Volume}}" rejected
check "type ObjectStore without field"  "$REH  snapshot: {from: {type: ObjectStore}}" rejected
check "type Custom without field"       "$REH  snapshot: {from: {type: Custom}}" rejected
check "HTTP with a non-http url"        "$REH  snapshot: {from: {type: HTTP, http: {url: ftp://x/y}}}" rejected
check "a valid Volume provider"         "$REH$VOL" ok

echo "-- addons --"
check "Enterprise with no addons source" "$REH$VOL
  addons: {edition: Enterprise}" rejected
check "Enterprise with a repo"           "$REH$VOL
  addons: {edition: Enterprise, repos: [{name: ent, url: https://github.com/odoo/enterprise}]}" ok
check "a repo name that is not DNS-safe" "$REH$VOL
  addons: {repos: [{name: MyRepo, url: u}]}" rejected

echo "-- enums --"
check "a non-existent engine"    "$REH$VOL
  migration: {engine: Whatever}" rejected
check "a non-existent auth type" "$REH$VOL
  addons: {repos: [{name: r, url: u, auth: {type: Magic, secretRef: s}}]}" rejected
check "a non-existent size"      "$REH$VOL
  size: enormous" rejected
check "a non-existent format"    "$REH  snapshot: {from: {type: Volume, volume: {claimName: d}}, format: Zip}" rejected

echo "-- public environment: the same security as production --"
check "public without authentication"   "$ENV  exposure: {public: true, host: h.x, auth: {type: None}}
  data: {type: Snapshot, snapshot: {from: {type: Volume, volume: {claimName: d}}}, acknowledgeReidentificationRisk: $ACK_REID}" rejected
check "public without a host"           "$ENV  exposure: {public: true}
  data: {type: Demo}" rejected
check "public without randomized pwds"  "$ENV  exposure: {public: true, host: h.x}
  security: {randomizeUserPasswords: false}
  data: {type: Snapshot, snapshot: {from: {type: Volume, volume: {claimName: d}}}, acknowledgeReidentificationRisk: $ACK_REID}" rejected
check "public with egress open"         "$ENV  exposure: {public: true, host: h.x}
  security: {denyEgress: false}
  data: {type: Snapshot, snapshot: {from: {type: Volume, volume: {claimName: d}}}, acknowledgeReidentificationRisk: $ACK_REID}" rejected
check "public without the reident ack"  "$ENV  exposure: {public: true, host: h.x}
  data: {type: Snapshot, snapshot: {from: {type: Volume, volume: {claimName: d}}}}" rejected
check "public, everything in order"     "$ENV  exposure: {public: true, host: h.x}
  data: {type: Snapshot, snapshot: {from: {type: Volume, volume: {claimName: d}}}, acknowledgeReidentificationRisk: $ACK_REID}" ok
check "private with real data"          "$ENV  data: {type: Snapshot, snapshot: {from: {type: Volume, volume: {claimName: d}}}}" ok
check "Snapshot without the snapshot field" "$ENV  data: {type: Snapshot}" rejected

echo "-- reading from production --"
check "live without acknowledgement" "$SNAP  source: {host: prod, user: u, passwordSecret: s, name: prod, live: true}" rejected
check "live with acknowledgement"    "$SNAP  source: {host: prod, user: u, passwordSecret: s, name: prod, live: true, acknowledgeProductionRead: $ACK_PROD}" ok
check "from a replica, no flag"      "$SNAP  source: {host: replica, user: u, passwordSecret: s, name: prod}" ok

RBL='apiVersion: doblura.dev/v1alpha1
kind: RunboatLink
metadata: {name: guardrail-check}
spec:
  url: https://runboat.example.com
'

echo "-- runboat link: two locks on the actions --"
# The first checks matter most, and they are not about rejecting anything.
#
# `self.allowedActions` on an object that OMITTED the field is an evaluation
# ERROR, not an empty list, and an erroring rule rejects EVERY object of the
# kind. So a plain read-only link being accepted is what proves the has() guard
# is really there — the same trap that took out the whole OdooDatabase CRD once,
# and that a grep over the rule text cannot tell you anything about.
check "a read-only link, nothing declared" "$RBL" ok
check "allowedActions, no requests"        "$RBL  allowedActions: [Start, Stop]" ok

# Then the rule itself.
check "request with allowedActions absent" "$RBL  auth: {basicAuthSecret: s}
  actionRequests: [{id: a1, build: b, action: Reset}]" rejected
check "request for an unlisted action"     "$RBL  auth: {basicAuthSecret: s}
  allowedActions: [Stop]
  actionRequests: [{id: a1, build: b, action: Reset}]" rejected
check "request with no auth at all"        "$RBL  allowedActions: [Reset]
  actionRequests: [{id: a1, build: b, action: Reset}]" rejected
check "request, listed and authenticated"  "$RBL  auth: {basicAuthSecret: s}
  allowedActions: [Reset, Stop]
  actionRequests: [{id: a1, build: b, action: Reset}]" ok

# Undeploy is absent from the enum on purpose: runboat's bulk delete takes the
# same shared credential and a filter, and there is no version of "undeploy every
# build for this repo" that belongs behind a console button.
check "Undeploy is not an action"          "$RBL  allowedActions: [Undeploy]" rejected
check "url without a scheme"              'apiVersion: doblura.dev/v1alpha1
kind: RunboatLink
metadata: {name: guardrail-check}
spec: {url: runboat.example.com}' rejected

TEN='apiVersion: doblura.dev/v1alpha1
kind: OdooTenant
metadata: {name: guardrail-check}
spec:
  displayName: Guardrail
'

echo "-- the module set the rehearsal actually exercised --"
check "modules: an empty assertion"    "$REH$VOL
  assertions: {modules: {}}" rejected
check "modules: a list"                "$REH$VOL
  assertions: {modules: {installed: [account, stock]}}" ok
check "modules: a minimum"             "$REH$VOL
  assertions: {modules: {minCount: 100}}" ok
check "modules: a minimum of zero"     "$REH$VOL
  assertions: {modules: {minCount: 0}}" rejected

echo "-- the filestore, which is state outside the database --"
# The first two expect OK. They are what proves the has() guards are present: a
# rule that errors on an absent lifecycle rejects every environment, and that is
# invisible from the rejection side.
check "ephemeral env, ephemeral filestore"   "$ENV  lifecycle: {ttl: 8h}
  data: {type: Demo}" ok
check "persistent env, no storage declared"  "$ENV  lifecycle: {type: Persistent}
  data: {type: Demo}" ok
check "persistent env, EPHEMERAL filestore"  "$ENV  lifecycle: {type: Persistent}
  data: {type: Demo}
  storage: {filestore: {mode: Ephemeral}}" rejected
check "hibernating env, EPHEMERAL filestore" "$ENV  lifecycle: {type: Hibernating}
  data: {type: Demo}
  storage: {filestore: {mode: Ephemeral}}" rejected
check "persistent env, PVC filestore"        "$ENV  lifecycle: {type: Persistent}
  data: {type: Demo}
  storage: {filestore: {mode: PersistentVolumeClaim, claimName: fs}}" ok
check "PVC filestore with neither claim nor size" "$ENV  data: {type: Demo}
  storage: {filestore: {mode: PersistentVolumeClaim}}" rejected
check "claimName with mode Ephemeral"        "$ENV  lifecycle: {ttl: 8h}
  data: {type: Demo}
  storage: {filestore: {mode: Ephemeral, claimName: fs}}" rejected

# Database mode: Odoo core's ir_attachment.location, no addon and no volume.
check "persistent env, Database filestore"   "$ENV  lifecycle: {type: Persistent}
  data: {type: Demo}
  storage: {filestore: {mode: Database}}" ok
check "Database filestore with a claimName"  "$ENV  data: {type: Demo}
  storage: {filestore: {mode: Database, claimName: fs}}" rejected
check "Database filestore with a size"       "$ENV  data: {type: Demo}
  storage: {filestore: {mode: Database, size: 20Gi}}" rejected

echo "-- replicas and the filestore they share --"
check "2 replicas, ephemeral filestore"      "$ENV  data: {type: Demo}
  storage: {filestore: {mode: PersistentVolumeClaim, claimName: fs}}
  workload: {web: {replicas: 2}}" rejected
check "2 replicas, PVC declared RWX"         "$ENV  data: {type: Demo}
  storage: {filestore: {mode: PersistentVolumeClaim, claimName: fs, accessModeReadWriteMany: true}}
  workload: {web: {replicas: 2}}" ok
check "1 replica needs no RWX"               "$ENV  data: {type: Demo}
  storage: {filestore: {mode: PersistentVolumeClaim, claimName: fs}}
  workload: {web: {replicas: 1}}" ok
# Database mode has no filestore to share, so many replicas need no RWX at all.
check "3 replicas, Database filestore"       "$ENV  data: {type: Demo}
  storage: {filestore: {mode: Database}}
  workload: {web: {replicas: 3}}" ok

echo "-- the connection proxy --"
# Mode is the only field with a safe default, so an absent proxy block and an
# explicit None must both be accepted without dragging anything else in.
check "no proxy block"                       "$ENV  data: {type: Demo}" ok
check "proxy mode None"                      "$ENV  data: {type: Demo}
  database: {host: h, user: u, passwordSecret: s, proxy: {mode: None}}" ok
check "Sidecar without an image"             "$ENV  data: {type: Demo}
  database: {host: h, user: u, passwordSecret: s, proxy: {mode: Sidecar}}" rejected
check "Sidecar with an image"                "$ENV  data: {type: Demo}
  database: {host: h, user: u, passwordSecret: s, proxy: {mode: Sidecar, image: pgb:1}}" ok
# Transaction pooling loses Odoo's bus. The rule guards on has() first: without
# it CEL raises "no such key" instead of failing validation, which rejects for
# the wrong reason and says nothing useful.
check "Transaction without acknowledgement"  "$ENV  data: {type: Demo}
  database: {host: h, user: u, passwordSecret: s, proxy: {mode: Sidecar, image: pgb:1, poolMode: Transaction}}" rejected
check "Transaction with a wrong value"       "$ENV  data: {type: Demo}
  database: {host: h, user: u, passwordSecret: s, proxy: {mode: Sidecar, image: pgb:1, poolMode: Transaction, unsafeAcknowledgement: yes-ok}}" rejected
check "Transaction acknowledged"             "$ENV  data: {type: Demo}
  database: {host: h, user: u, passwordSecret: s, proxy: {mode: Sidecar, image: pgb:1, poolMode: Transaction, unsafeAcknowledgement: i-accept-transaction-pooling-breaks-the-odoo-bus}}" ok
check "Session needs no acknowledgement"     "$ENV  data: {type: Demo}
  database: {host: h, user: u, passwordSecret: s, proxy: {mode: Sidecar, image: pgb:1, poolMode: Session}}" ok

echo "-- crons and jobs --"
# A cron tier is a SECOND pod writing the filestore, so it carries the same
# sharing requirement as a second web replica. These checks exist because the
# feature shipped once with the API accepting the combination and the operator
# creating two pods over one ReadWriteOnce volume, which works right up to the
# first reschedule.
check "cron tier, ephemeral filestore"       "$ENV  data: {type: Demo}
  storage: {filestore: {mode: Ephemeral}}
  lifecycle: {ttl: 8h}
  workload: {cron: {replicas: 1}}" rejected
check "cron tier, RWO PVC filestore"         "$ENV  data: {type: Demo}
  storage: {filestore: {mode: PersistentVolumeClaim, claimName: fs}}
  workload: {cron: {replicas: 1}}" rejected
check "cron tier, PVC declared RWX"          "$ENV  data: {type: Demo}
  storage: {filestore: {mode: PersistentVolumeClaim, claimName: fs, accessModeReadWriteMany: true}}
  workload: {cron: {replicas: 1}}" ok
check "cron tier, Database filestore"        "$ENV  data: {type: Demo}
  storage: {filestore: {mode: Database}}
  workload: {cron: {replicas: 1}}" ok
# replicas 0 is "no cron tier", so it must not drag the filestore rule in.
check "cron tier disabled needs no sharing"  "$ENV  data: {type: Demo}
  storage: {filestore: {mode: Ephemeral}}
  lifecycle: {ttl: 8h}
  workload: {cron: {replicas: 0}}" ok

check "cron tier with 2 replicas"            "$ENV  data: {type: Demo}
  workload: {cron: {replicas: 2}}" rejected
# Carries a shareable filestore: with the default (Ephemeral) this is now
# rejected, and rightly — the check is about cron replicas, not the filestore.
check "cron tier with 1"                     "$ENV  data: {type: Demo}
  storage: {filestore: {mode: Database}}
  workload: {cron: {replicas: 1}}" ok
check "queue_job runner with 2"              "$ENV  data: {type: Demo}
  workload: {queueJob: {replicas: 2}}" rejected

echo

echo "-- the tenant quota field --"
check "a negative quota"            "$TEN  maxEphemeralEnvironments: -1" rejected
check "a quota of zero"             "$TEN  maxEphemeralEnvironments: 0" ok
check "a tenant with no quota at all" "$TEN" ok

# And the DEFAULT, asserted against the API server rather than against the marker.
#
# The number lives twice — the kubebuilder default the API server applies, and the
# Go constant the webhook falls back to — and a drift between them is invisible.
# A unit test compares the constant with the generated CRD; this compares the
# generated CRD with what a cluster actually does with it.
defaulted=$(printf '%s' "$TEN" | kubectl create --dry-run=server -f - \
  -o jsonpath='{.spec.maxEphemeralEnvironments}' 2>/dev/null)
if [ "$defaulted" = "3" ]; then printf '  ok    a tenant with no quota is defaulted to 3\n'
else printf '  FAIL  a tenant with no quota was defaulted to %s, expected 3\n' "${defaulted:-<nothing>}"; fails=$((fails+1)); fi

# Zero has to SURVIVE. It is the answer that says "no throwaway copy of this
# customer's data, ever", and a field that quietly turns it into the default would
# grant exactly what somebody tried to deny.
zero=$(printf '%s' "$TEN  maxEphemeralEnvironments: 0" | kubectl create --dry-run=server -f - \
  -o jsonpath='{.spec.maxEphemeralEnvironments}' 2>/dev/null)
if [ "$zero" = "0" ]; then printf '  ok    an explicit quota of zero is not replaced by the default\n'
else printf '  FAIL  an explicit quota of zero came back as %s\n' "${zero:-<nothing>}"; fails=$((fails+1)); fi

# ─────────────── the quota webhook ───────────────
#
# Everything above runs against the CRDs alone. This part needs the operator
# serving admission, so it runs only where the webhook is installed — `make
# e2e-quota`, which does a real helm install with a real image.
#
# It SAYS when it is skipping. A quota check that silently does nothing in the
# cluster where it matters is the failure mode this project keeps rediscovering.
echo
echo "-- the image catalogue --"
check "one default"                          "$TEN  images:
    - {name: a, image: 'x:1', odooVersion: '18.0', default: true}
    - {name: b, image: 'x:2', odooVersion: '18.1'}" ok
# Two defaults means which image a new environment gets depends on list order,
# and list order is not something anybody edits on purpose.
check "two defaults"                         "$TEN  images:
    - {name: a, image: 'x:1', odooVersion: '18.0', default: true}
    - {name: b, image: 'x:2', odooVersion: '18.1', default: true}" rejected
check "no default is allowed"                "$TEN  images:
    - {name: a, image: 'x:1', odooVersion: '18.0'}" ok
# listType=map keyed on name: the API server enforces uniqueness itself, which
# is why there is no CEL rule doing it in quadratic time.
check "duplicate catalogue names"            "$TEN  images:
    - {name: a, image: 'x:1', odooVersion: '18.0'}
    - {name: a, image: 'x:2', odooVersion: '18.1'}" rejected
# The version is declared, never parsed from the tag: hms:18, hms:18.0-rc2 and
# hms:stable may all be Odoo 18, and guessing is how a major slips through.
check "a version that is not a version"      "$TEN  images:
    - {name: a, image: 'x:1', odooVersion: latest}" rejected
check "a name that is not a DNS label"       "$TEN  images:
    - {name: 'Not A Name', image: 'x:1', odooVersion: '18.0'}" rejected
check "majorUpgrade naming no entry"         "$TEN  images:
    - {name: a, image: 'x:1', odooVersion: '18.0', default: true}
  majorUpgrade: {toImage: nope, rehearsalRef: r, acknowledgement: i-accept-a-major-upgrade-rewrites-the-database-and-cannot-be-rolled-back}" rejected
check "majorUpgrade without the literal"     "$TEN  images:
    - {name: a, image: 'x:1', odooVersion: '18.0', default: true}
  majorUpgrade: {toImage: a, rehearsalRef: r, acknowledgement: sure}" rejected
# A fresh customer record is not an upgrade: there is nothing to cross FROM, and
# demanding a rehearsal would be asking for evidence about a migration that is
# not happening.
check "a new customer on any major"          "$TEN  images:
    - {name: a, image: 'x:1', odooVersion: '19.0', default: true}" ok
echo
WHC=$(kubectl get validatingwebhookconfiguration -l app.kubernetes.io/name=doblura \
  -o jsonpath='{.items[0].metadata.name}' 2>/dev/null)
if [ -z "$WHC" ]; then
  echo "-- environment quota: SKIPPED (no quota webhook in this cluster; run make e2e-quota) --"
  echo

[ $fails -eq 0 ] && echo "  all guardrails OK" || { echo "  $fails guardrail(s) failed"; exit 1; }
  exit 0
fi

echo "-- environment quota (webhook $WHC) --"

# The configuration is installed, so SOMETHING has to be serving it. If the manager
# is not ready, every check below fails on a TLS error, and the run is a page of
# noise that looks like a quota bug. This is a precondition, not a skip: a
# fail-closed webhook with nothing behind it is itself the problem.
ready=$(kubectl get deploy -A -l app.kubernetes.io/name=doblura -o jsonpath='{.items[0].status.readyReplicas}' 2>/dev/null)
if [ "${ready:-0}" -lt 1 ] 2>/dev/null; then
  printf '  FAIL  the quota webhook is installed but the manager has no ready replica:\n'
  kubectl get pods -A -l app.kubernetes.io/name=doblura 2>&1 | sed 's/^/         /'
  echo
  echo "  $((fails+1)) guardrail(s) failed"
  exit 1
fi

NS=quota-guardrail
ALICE=alice@example.com
BRUNO=bruno@example.com
OPERATOR=$(kubectl get deploy -A -l app.kubernetes.io/name=doblura \
  -o jsonpath='system:serviceaccount:{.items[0].metadata.namespace}:{.items[0].spec.template.spec.serviceAccountName}' 2>/dev/null)

kubectl delete namespace $NS --ignore-not-found --wait=false >/dev/null 2>&1
kubectl wait --for=delete namespace/$NS --timeout=120s >/dev/null 2>&1
if ! kubectl create namespace $NS >/dev/null 2>&1; then
  # Usually the previous run's namespace still terminating, which means the
  # environments in it still have their finalizers. Say so instead of running
  # every check against a namespace that is going away.
  printf '  FAIL  could not create the %s namespace (still terminating from a previous run?)\n' "$NS"
  echo; echo "  $((fails+1)) guardrail(s) failed"; exit 1
fi

# The two people are bound to the SUPPORT persona, which is the role this quota
# exists for: it can create and delete ephemeral environments and nothing else.
#
# The first version of this section impersonated users with no RBAC at all, and
# every "rejected" expectation passed — on a Forbidden from RBAC, before the webhook
# was ever consulted. Half the section was green and none of it tested the quota.
SUPPORT_ROLE=$(kubectl get clusterrole -l doblura.dev/persona=support -o jsonpath='{.items[0].metadata.name}' 2>/dev/null)
if [ -z "$SUPPORT_ROLE" ]; then
  printf '  FAIL  no support persona ClusterRole in this cluster\n'; fails=$((fails+1))
fi
for u in $ALICE $BRUNO; do
  kubectl create clusterrolebinding "quota-guardrail-${u%%@*}" \
    --clusterrole="$SUPPORT_ROLE" --user="$u" >/dev/null 2>&1
done

# live: create the object FOR REAL as somebody, and say whether it landed.
#
# Impersonation is how a real person's identity reaches the webhook without five
# kubeconfigs: UserInfo is filled in by the API server either way.
#
# `denied` requires the words the admission webhook puts there. Anything else that
# refuses a create — RBAC, a CEL rule, a typo in the manifest — produces a
# different error, and lumping those together is how this section passed while
# testing nothing.
live() { # name, user, yaml, ok|denied
  out=$(printf '%s' "$3" | kubectl apply --as "$2" -n $NS -f - 2>&1)
  if printf '%s' "$out" | grep -q 'created\|configured'; then r=ok
  elif printf '%s' "$out" | grep -q 'denied the request'; then r=denied
  else r="another error entirely"; fi
  if [ "$4" = "$r" ]; then printf '  ok    %s\n' "$1"
  else printf '  FAIL  %s: %s (expected %s)\n         %s\n' "$1" "$r" "$4" "$out"; fails=$((fails+1)); fi
  LAST_OUT="$out"
}

env_for() { # name, tenant
  printf 'apiVersion: doblura.dev/v1alpha1\nkind: OdooEnvironment\nmetadata:\n  name: %s\n  annotations:\n    doblura.dev/created-by: forged@example.com\nspec:\n  image: odoo:19.0\n  forTenant: %s\n  data: {type: Demo}\n  database: {host: pg, user: odoo, passwordSecret: pg}\n' "$1" "$2"
}

kubectl apply -n $NS -f - >/dev/null <<'YAML'
apiVersion: doblura.dev/v1alpha1
kind: OdooTenant
metadata: {name: acme}
spec: {displayName: Acme, maxEphemeralEnvironments: 1}
---
apiVersion: doblura.dev/v1alpha1
kind: OdooTenant
metadata: {name: globex}
spec: {displayName: Globex, maxEphemeralEnvironments: 3}
YAML

# THE CHECK THAT MATTERS MOST, and it is not a rejection: a webhook that refuses
# everything looks perfectly healthy from the rejection side. If this one fails,
# nothing else in this section means anything.
live "a first environment is created normally" "$ALICE" "$(env_for acme-1 acme)" ok

# The creator is whoever the API SERVER says it is. The manifest above claims
# forged@example.com on purpose.
stamped=$(kubectl get odooenvironment acme-1 -n $NS -o jsonpath='{.metadata.annotations.doblura\.dev/created-by}')
if [ "$stamped" = "$ALICE" ]; then printf '  ok    the creator annotation is stamped by the server, not taken from the request\n'
else printf '  FAIL  created-by is %q, expected %q (a forged value was trusted)\n' "$stamped" "$ALICE"; fails=$((fails+1)); fi

live "the second environment for a customer capped at one" "$ALICE" "$(env_for acme-2 acme)" denied
echo "         ↳ $(printf '%s' "$LAST_OUT" | tr '\n' ' ' | cut -c1-320)"

# The per-customer limit is per CUSTOMER: somebody else does not get a fresh three.
live "somebody else, same customer, still refused" "$BRUNO" "$(env_for acme-3 acme)" denied

# A different customer with room is unaffected — the refusal above was about a
# quota, not about the webhook being broken.
live "a different customer with room" "$ALICE" "$(env_for globex-1 globex)" ok

# The per-person allowance, which is what stops fifty environments spread over
# twenty customers. make e2e-quota installs the chart with 2.
PERSONAL=$(kubectl get deploy -A -l app.kubernetes.io/name=doblura \
  -o jsonpath='{range .items[0].spec.template.spec.containers[0].args[*]}{@}{"\n"}{end}' \
  | sed -n 's/^--max-environments-per-creator=//p')
if [ "$PERSONAL" = "2" ]; then
  live "the third environment for one person, across customers" "$ALICE" "$(env_for globex-2 globex)" denied
  echo "         ↳ $(printf '%s' "$LAST_OUT" | tr '\n' ' ' | cut -c1-320)"
  live "and somebody else still has their own allowance" "$BRUNO" "$(env_for globex-3 globex)" ok
else
  printf '  skip  the per-person limit (the release is configured for %s, this check needs 2)\n' "${PERSONAL:-?}"
fi

# The operator creates environments on the cluster's behalf and must never be
# throttled by a person's limit. acme is full, so this is the exemption or nothing.
if [ -n "$OPERATOR" ]; then
  live "the operator's own ServiceAccount is exempt" "$OPERATOR" "$(env_for acme-operator acme)" ok
else
  printf '  FAIL  could not find the operator ServiceAccount to test the exemption\n'; fails=$((fails+1))
fi

# A Persistent environment is somebody's staging: not a throwaway, not capped.
live "a Persistent environment is not capped" "$ALICE" "$(printf 'apiVersion: doblura.dev/v1alpha1\nkind: OdooEnvironment\nmetadata: {name: acme-staging}\nspec:\n  image: odoo:19.0\n  forTenant: acme\n  lifecycle: {type: Persistent}\n  data: {type: Demo}\n  database: {host: pg, user: odoo, passwordSecret: pg}\n')" ok

# Deleting is how a full quota is cleared, so it must work while the quota is full:
# the webhook is on CREATE only, deliberately.
if kubectl delete odooenvironment acme-1 -n $NS --as "$ALICE" --wait=false >/dev/null 2>&1; then
  printf '  ok    a full quota can still be cleared: DELETE is not intercepted\n'
else
  printf '  FAIL  could not delete an environment while the quota was full\n'; fails=$((fails+1))
fi

# Clean up in the right order: the environments first, while the operator is still
# there to take their finalizers off. Deleting the namespace on its own leaves them
# terminating for as long as nothing reconciles them, and the next run then finds a
# namespace it cannot create.
kubectl delete odooenvironment --all -n $NS --timeout=60s >/dev/null 2>&1
kubectl delete namespace $NS --wait=false >/dev/null 2>&1
for u in $ALICE $BRUNO; do
  kubectl delete clusterrolebinding "quota-guardrail-${u%%@*}" --ignore-not-found >/dev/null 2>&1
done

[ $fails -eq 0 ] && echo "  all guardrails OK" || { echo "  $fails guardrail(s) failed"; exit 1; }
