#!/usr/bin/env bash
# Check that the API server rejects what it should reject.
# The guardrails live in the CRDs, not in the documentation: this proves it.
set -uo pipefail
fails=0

# The manager has to be up before anything below means anything.
#
# Its webhooks are failurePolicy: Fail, so while it is rolling out EVERY create is
# rejected — and this script reports that as a page of guardrails failing, which
# reads like the rules broke rather than like the pod is thirty seconds old. That
# happened, and it cost a real investigation before the pod finished starting.
for _ in $(seq 1 60); do
  ready=$(kubectl get deploy -A -l app.kubernetes.io/name=doblura \
    -o jsonpath='{.items[0].status.readyReplicas}' 2>/dev/null)
  [ "${ready:-0}" -ge 1 ] 2>/dev/null && break
  sleep 2
done
if [ "${ready:-0}" -lt 1 ] 2>/dev/null; then
  printf '  the manager has no ready replica, so every check below would fail on a\n'
  printf '  webhook that is not answering rather than on the rule it is testing:\n'
  kubectl get pods -A -l app.kubernetes.io/name=doblura 2>&1 | sed 's/^/    /'
  exit 1
fi

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
echo "-- outgoing mail, the one setting that reaches the customer's customers --"
# A working SMTP server on a copy of production sends real invoices to real
# people from a machine nobody is watching, and there is no undo.
check "mail on a review env, no ack"        "$ENV  purpose: Review
  data: {type: Snapshot, snapshot: {from: {type: Volume, volume: {claimName: d}}}}
  mail: {host: smtp.example.com}" rejected
check "mail on a review env, wrong ack"     "$ENV  purpose: Review
  data: {type: Snapshot, snapshot: {from: {type: Volume, volume: {claimName: d}}}}
  mail: {host: smtp.example.com, unsafeAcknowledgement: yes-i-know}" rejected
check "mail on a review env, acknowledged"  "$ENV  purpose: Review
  data: {type: Snapshot, snapshot: {from: {type: Volume, volume: {claimName: d}}}}
  mail: {host: smtp.example.com, unsafeAcknowledgement: i-accept-this-environment-can-send-real-email-to-real-people}" ok
# Production is where mail belongs, and needs no ceremony to have it.
check "mail on production needs no ack"     "$ENV  purpose: Production
  data: {type: Live}
  lifecycle: {type: Persistent}
  storage: {filestore: {mode: Database}}
  mail: {host: smtp.example.com}" ok
# Demo data has no real addresses; every message goes to an invented one.
check "mail with demo data"                 "$ENV  purpose: Production
  data: {type: Demo}
  mail: {host: smtp.example.com}" rejected
# An SMTP user with no password is a login that cannot log in.
check "smtp user with no password"          "$ENV  purpose: Production
  data: {type: Live}
  lifecycle: {type: Persistent}
  storage: {filestore: {mode: Database}}
  mail: {host: smtp.example.com, smtpUser: odoo}" rejected

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

# ── the default customer ──
#
# For the company that runs its own Odoo rather than forty of somebody else's.
# Without it that company gets none of the platform — no image catalogue, no
# generated address, no defaults — unless it writes forTenant on every environment
# for ever. One record, marked once.
echo "-- the default customer --"

DNS=default-tenant-guardrail
kubectl delete namespace $DNS --ignore-not-found --wait=false >/dev/null 2>&1
kubectl wait --for=delete namespace/$DNS --timeout=120s >/dev/null 2>&1
kubectl create namespace $DNS >/dev/null 2>&1

mkten() { # name, default(true|false)
  kubectl apply -f - >/dev/null 2>&1 <<YAML
apiVersion: doblura.dev/v1alpha1
kind: OdooTenant
metadata: {name: $1, namespace: $DNS}
spec:
  displayName: $1
  default: $2
  domain: $1.example.com
  images:
    - {name: v18, image: 'odoo:18.0', odooVersion: '18.0', default: true}
  environmentDefaults:
    database: {host: pg, user: odoo, passwordSecret: pg}
    storage: {filestore: {mode: Database}}
    size: small
YAML
}

mkten uno true
if kubectl -n $DNS get odootenant uno >/dev/null 2>&1; then
  printf '  ok    one customer can be the default\n'
else
  printf '  FAIL  a customer could not be marked as the default\n'; fails=$((fails+1))
fi

# The second one is refused HERE, when it is marked — not later, at somebody
# else's environment, about a record they never touched.
out=$(kubectl apply -f - 2>&1 <<YAML
apiVersion: doblura.dev/v1alpha1
kind: OdooTenant
metadata: {name: dos, namespace: $DNS}
spec:
  displayName: dos
  default: true
YAML
)
if printf '%s' "$out" | grep -q 'already the default customer'; then
  printf '  ok    a second default is refused when it is marked\n'
else
  printf '  FAIL  a second default customer: %s\n' "$(printf '%s' "$out" | head -c 120)"
  fails=$((fails+1))
fi

# And the point of the whole thing: an environment that declares almost nothing.
kubectl apply -f - >/dev/null 2>&1 <<YAML
apiVersion: doblura.dev/v1alpha1
kind: OdooEnvironment
metadata: {name: nada-declarado, namespace: $DNS}
spec:
  purpose: Staging
  data: {type: Demo}
YAML
got=$(kubectl -n $DNS get odooenvironment nada-declarado \
  -o jsonpath='{.spec.forTenant}|{.spec.image}|{.spec.database.host}|{.spec.size}' 2>/dev/null)
case "$got" in
  "uno|odoo:18.0|pg|small") printf '  ok    an environment declaring nothing inherits everything\n' ;;
  *) printf '  FAIL  an environment inherited %s (expected uno|odoo:18.0|pg|small)\n' "${got:-nothing}"
     fails=$((fails+1)) ;;
esac

# It gets an address under the customer's domain, and the random tail is what
# stops it being found by typing the obvious name.
host=$(kubectl -n $DNS get odooenvironment nada-declarado \
  -o jsonpath='{.spec.exposure.host}' 2>/dev/null)
case "$host" in
  nada-declarado-*.uno.example.com)
    [ "$host" != "nada-declarado.uno.example.com" ] &&
      printf '  ok    and an address that cannot be guessed\n' ||
      { printf '  FAIL  the address is the predictable one: %s\n' "$host"; fails=$((fails+1)); } ;;
  *) printf '  FAIL  the address is %s\n' "${host:-empty}"; fails=$((fails+1)) ;;
esac

kubectl delete namespace $DNS --ignore-not-found --wait=false >/dev/null 2>&1
echo

# ── the Odoo versions this release supports ──
#
# DECISIONS.md 15: the three majors Odoo supports, plus the one that just fell out,
# for a year. The grace year is the whole point — doblura exists to rehearse the
# migration OFF an old version, and dropping 17 the week Odoo does would remove the
# tool from exactly the people who need it.
#
# Checked here rather than written in a document, because a support policy nothing
# enforces is a support policy that drifts one accepted image at a time.
echo "-- the Odoo versions this release supports --"

VNS=version-guardrail
kubectl delete namespace $VNS --ignore-not-found --wait=false >/dev/null 2>&1
kubectl wait --for=delete namespace/$VNS --timeout=120s >/dev/null 2>&1
kubectl create namespace $VNS >/dev/null 2>&1

ver() { # label, version, ok|rejected
  out=$(kubectl apply -f - 2>&1 <<YAML
apiVersion: doblura.dev/v1alpha1
kind: OdooTenant
metadata: {name: v$(echo "$2" | tr -d '.'), namespace: $VNS}
spec:
  displayName: "Odoo $2"
  images:
    - {name: only, image: 'odoo:$2', odooVersion: '$2', default: true}
YAML
)
  if printf '%s' "$out" | grep -q 'created\|configured'; then r=ok; else r=rejected; fi
  if [ "$3" = "$r" ]; then printf '  ok    %s\n' "$1"
  else printf '  FAIL  %s: %s (expected %s)\n' "$1" "$r" "$3"; fails=$((fails+1)); fi
}

# Supported by Odoo today, and the one in its grace year.
ver "19.0, current"                19.0 ok
ver "18.0, supported"              18.0 ok
ver "17.0, supported"              17.0 ok
ver "16.0, in its grace year"      16.0 ok
# A version that is not a version at all. The catalogue takes an Odoo major, and
# "latest" in a field somebody reads to decide what is running is how two people
# end up disagreeing about which product a customer is on.
ver "a version that is not one"    latest rejected

kubectl delete namespace $VNS --ignore-not-found --wait=false >/dev/null 2>&1
echo

# ── what the data is, and what follows from it ──
#
# Doblura cannot make anybody compliant with anything. What it can do is refuse the
# handful of configurations that are wrong whichever version of whichever standard
# applies, and have the evidence collected before somebody asks. These check the
# refusals — a control is a control because it refuses, and a paragraph in a policy
# document is not.
echo "-- what the data is --"

CNSD=data-guardrail
kubectl delete namespace $CNSD --ignore-not-found --wait=false >/dev/null 2>&1
kubectl wait --for=delete namespace/$CNSD --timeout=120s >/dev/null 2>&1
kubectl create namespace $CNSD >/dev/null 2>&1
kubectl apply -f - >/dev/null 2>&1 <<YAML
apiVersion: doblura.dev/v1alpha1
kind: OdooTenant
metadata: {name: regulado, namespace: $CNSD}
spec:
  displayName: regulado
  default: true
  holds: {personalData: true, cardholderData: true}
  images:
    - {name: v18, image: 'odoo:18.0', odooVersion: '18.0', default: true}
  environmentDefaults:
    database: {host: pg, user: odoo, passwordSecret: pg}
    storage: {filestore: {mode: Database}}
    size: small
YAML

denv() { # label, extra-spec, ok|rejected, [expected words]
  name="d-$(echo "$1" | tr -cd 'a-z0-9')"
  out=$(kubectl apply -f - 2>&1 <<YAML
apiVersion: doblura.dev/v1alpha1
kind: OdooEnvironment
metadata: {name: $name, namespace: $CNSD}
spec:
$2
YAML
)
  if printf '%s' "$out" | grep -q 'created'; then r=ok; else r=rejected; fi
  if [ "$3" != "$r" ]; then
    printf '  FAIL  %s: %s (expected %s)\n         %s\n' "$1" "$r" "$3" \
      "$(printf '%s' "$out" | sed 's/.*denied the request: //' | head -c 130)"
    fails=$((fails+1)); return
  fi
  if [ -n "${4:-}" ] && ! printf '%s' "$out" | grep -qi "$4"; then
    printf '  FAIL  %s: refused for the wrong reason: %s\n' "$1" \
      "$(printf '%s' "$out" | sed 's/.*denied the request: //' | head -c 130)"
    fails=$((fails+1)); return
  fi
  printf '  ok    %s\n' "$1"
}

# Cardholder data outside production, with no acknowledgement available. It is a
# scope argument, not a safety one: the copy puts its environment, its cluster and
# its backups inside the audit.
denv "live cardholder data outside production" \
  "  purpose: Staging
  data: {type: Live}
  lifecycle: {type: Persistent}" rejected "cardholder data"
denv "anonymised is how you do that instead" \
  "  purpose: Staging
  data: {type: Snapshot, snapshot: {from: {type: Volume, volume: {claimName: b}}}}
  lifecycle: {type: Persistent}" ok
denv "mail outside production, even acknowledged" \
  "  purpose: Staging
  data: {type: Snapshot, snapshot: {from: {type: Volume, volume: {claimName: b}}}}
  lifecycle: {type: Persistent}
  mail:
    host: smtp.example.com
    port: 25
    unsafeAcknowledgement: i-accept-this-environment-can-send-real-email-to-real-people" \
  rejected "cardholder data"
# An environment with real people's details indexed by a search engine is a
# disclosure nobody had to attack anything to get.
denv "noindex cannot be turned off" \
  "  purpose: QA
  data: {type: Demo}
  exposure: {noIndex: false}" rejected "personal data"

# And the evidence: what holds regulated data is a label, so it can be selected on
# rather than inferred from a customer record that may have changed since.
got=$(kubectl -n $CNSD get odooenvironment d-anonymisedishowyoudothatinstead \
  -o jsonpath='{.metadata.labels.doblura\.dev/data}' 2>/dev/null)
if [ "$got" = "PersonalData_CardholderData" ]; then
  printf '  ok    an environment records what it holds\n'
else
  printf '  FAIL  the environment records %s\n' "${got:-nothing}"; fails=$((fails+1))
fi

kubectl delete namespace $CNSD --ignore-not-found --wait=false >/dev/null 2>&1
echo

# ── the edge: what stands between the internet and an Odoo ──
#
# These exist because spec.exposure was a set of fields nothing applied. The
# Ingress referenced Traefik middlewares — basicauth, noindex, ratelimit — that no
# controller created; Traefik logged `middleware "..." does not exist` on every
# reconcile; and a public environment's authentication was enforced by nobody. The
# validation below is about the API. Whether the rules actually reach the proxy is
# checked against a live Traefik, which a guardrail script cannot do.
echo "-- the edge --"

check "a domain the customer can be published under" "$TEN  domain: acme.example.com" ok
check "a domain that is a URL, not a domain"         "$TEN  domain: https://acme.example.com" rejected
check "a domain with no dot at all"                  "$TEN  domain: localhost" rejected
check "an issuer names its kind"                     "$TEN  certIssuer: ClusterIssuer/letsencrypt" ok
check "a namespaced issuer too"                      "$TEN  certIssuer: Issuer/internal-ca" ok
check "an issuer with no kind is refused"            "$TEN  certIssuer: letsencrypt" rejected
# Naming the kind is not pedantry: cert-manager looks the two up in different
# places, and a name alone would be resolved by guessing.
check "an issuer of an invented kind"                "$TEN  certIssuer: Wizard/merlin" rejected

check "the WAF can be off, and says so"    "$ENV  data: {type: Demo}
  exposure: {waf: {mode: None}}" ok
check "in-cluster inspection"             "$ENV  data: {type: Demo}
  exposure: {waf: {mode: InCluster, enforcement: Detect}}" ok
# Provider mode passes annotations to somebody else's controller. With none,
# doblura would report a WAF that nothing was asked to switch on.
check "provider mode needs annotations"   "$ENV  data: {type: Demo}
  exposure: {waf: {mode: Provider}}" rejected
check "provider mode with annotations"    "$ENV  data: {type: Demo}
  exposure: {waf: {mode: Provider, annotations: {'alb.ingress.kubernetes.io/wafv2-acl-arn': 'arn:x'}}}" ok
check "an enforcement that is not one"    "$ENV  data: {type: Demo}
  exposure: {waf: {mode: InCluster, enforcement: Maybe}}" rejected

check "an allowlist of networks"  "$ENV  data: {type: Demo}
  exposure: {allowFrom: ['203.0.113.0/24', '198.51.100.7/32']}" ok
check "hsts can be turned off"    "$ENV  data: {type: Demo}
  exposure: {hsts: false}" ok

echo

# ── the copy taken before a restore replaces a database ──
#
# A restore is the one action in doblura that destroys data on purpose. The
# acknowledgement naming the target catches a manifest copied from staging to
# production; it catches nothing at all against the other mistake, which is the
# right environment and the wrong copy. These check the rules that do.
#
# Real objects in a real namespace, because the webhook reads the TARGET to decide
# — that is the whole point of the field, and a dry-run against nothing would test
# the code path that refuses a missing environment.
echo "-- the copy taken before a restore --"

RNS=restore-guardrail
kubectl delete namespace $RNS --ignore-not-found --wait=false >/dev/null 2>&1
kubectl wait --for=delete namespace/$RNS --timeout=120s >/dev/null 2>&1
kubectl create namespace $RNS >/dev/null 2>&1

renv() { # name, purpose, lifecycle, dataType
  printf 'apiVersion: doblura.dev/v1alpha1
kind: OdooEnvironment
metadata: {name: %s, namespace: '"$RNS"'}
spec:
  forTenant: rg
  purpose: %s
  image: odoo:19.0
  size: small
  data: {type: %s}
  lifecycle: {type: %s}
  database: {host: pg, user: odoo, passwordSecret: pg}
' "$1" "$2" "$4" "$3"
}

kubectl apply -f - >/dev/null 2>&1 <<YAML
apiVersion: doblura.dev/v1alpha1
kind: OdooTenant
metadata: {name: rg, namespace: $RNS}
spec:
  displayName: Restore guardrails
  images:
    - {name: i, image: 'odoo:19.0', odooVersion: '19.0', default: true}
YAML
renv prod-none      Production Persistent Live | kubectl apply -f - >/dev/null 2>&1
renv prod-backed    Production Persistent Live | kubectl apply -f - >/dev/null 2>&1
renv stage-backed   Staging    Persistent Live | kubectl apply -f - >/dev/null 2>&1
renv review-nothing Review     Ephemeral  Demo | kubectl apply -f - >/dev/null 2>&1

for e in prod-backed stage-backed review-nothing; do
  kubectl apply -f - >/dev/null 2>&1 <<YAML
apiVersion: doblura.dev/v1alpha1
kind: OdooBackup
metadata: {name: b-$e, namespace: $RNS}
spec:
  environment: $e
  schedule: "0 2 * * *"
  suspend: true
  destination: {type: Volume, volume: {claimName: nothing}}
YAML
done

# rst: create a restore for real and report what admission decided, plus the value
# safetyCopy was resolved to. Dry-run would be enough for the refusals, but the
# resolved value only exists on an object the server actually wrote.
rst() { # label, into, extra-spec, ok|rejected, expected-safetyCopy-or-dash
  name="rg-$(echo "$1" | tr -cd 'a-z0-9')"
  out=$(kubectl apply -f - 2>&1 <<YAML
apiVersion: doblura.dev/v1alpha1
kind: OdooRestore
metadata: {name: $name, namespace: $RNS}
spec:
  backup: b-$2
  copy: 2026-01-01T00-00-00Z
  into: $2
  acknowledgement: i-accept-this-replaces-the-database-and-filestore-of-$2
$3
YAML
)
  if printf '%s' "$out" | grep -q 'created'; then r=ok; else r=rejected; fi
  if [ "$4" != "$r" ]; then
    printf '  FAIL  %s: %s (expected %s)\n         %s\n' "$1" "$r" "$4" \
      "$(printf '%s' "$out" | sed 's/.*denied the request: //' | head -c 160)"
    fails=$((fails+1)); return
  fi
  if [ "$5" != "-" ]; then
    got=$(kubectl -n $RNS get odoorestore "$name" -o jsonpath='{.spec.safetyCopy}' 2>/dev/null)
    if [ "$got" != "$5" ]; then
      printf '  FAIL  %s: safetyCopy resolved to %s (expected %s)\n' "$1" "${got:-unset}" "$5"
      fails=$((fails+1)); return
    fi
  fi
  printf '  ok    %s\n' "$1"
}

rst "production with nothing backing it up" prod-none  "" rejected -
rst "production may not turn the copy off"  prod-backed "  safetyCopy: false" rejected -
rst "production copies first"               prod-backed "" ok true
rst "staging copies first too"              stage-backed "" ok true
rst "a review environment does not"         review-nothing "" ok false

# The target has to exist. It is the field that decides whose database is replaced,
# and a typo in it must not produce an object that looks accepted.
out=$(kubectl apply -f - 2>&1 <<YAML
apiVersion: doblura.dev/v1alpha1
kind: OdooRestore
metadata: {name: rg-typo, namespace: $RNS}
spec:
  backup: b-prod-backed
  copy: 2026-01-01T00-00-00Z
  into: prod-backd
  acknowledgement: i-accept-this-replaces-the-database-and-filestore-of-prod-backd
YAML
)
if printf '%s' "$out" | grep -q 'no environment called'; then printf '  ok    a typo in the target is refused by name\n'
else printf '  FAIL  a typo in the target: %s\n' "$(printf '%s' "$out" | head -c 140)"; fails=$((fails+1)); fi

kubectl delete namespace $RNS --ignore-not-found --wait=false >/dev/null 2>&1
echo

# ── who may hand out access ──
#
# This section exists because the console now has a page that grants access, and a
# page that grants access is a privilege-escalation path if it is wrong. Nothing
# here tests the page: it tests what the API server would do to the person the page
# acts as, which is the only thing that actually decides.
#
# It goes ABOVE the quota section on purpose. That section starts with an early
# `exit 0` when no quota webhook is installed, and checks added below it have
# silently not run twice already.
echo "-- who may hand out access --"

ACCESS_NS=access-guardrail
kubectl create namespace $ACCESS_NS >/dev/null 2>&1 || true

role_for() { kubectl get clusterrole -l "doblura.dev/persona=$1" \
  -o jsonpath='{.items[0].metadata.name}' 2>/dev/null; }
PLAT=$(role_for platform); SUPP=$(role_for support); QA=$(role_for qa)

for p in $PLAT $SUPP; do
  kubectl create clusterrolebinding "access-guardrail-${p##*-}" \
    --clusterrole="$p" --group="access-guardrail-${p##*-}" >/dev/null 2>&1
done

# can: ask the API server whether a group may do something, as that group.
can() { # label, group, verb, resource, namespace-or-empty, yes|no
  args="--as-group=$2 --as=access-guardrail-probe"
  if [ -n "$5" ]; then args="$args -n $5"; else args="$args --all-namespaces"; fi
  # shellcheck disable=SC2086
  out=$(kubectl auth can-i "$3" "$4" $args 2>&1 | tr -d '[:space:]')
  case "$out" in yes*) r=yes ;; *) r=no ;; esac
  if [ "$6" = "$r" ]; then printf '  ok    %s\n' "$1"
  else printf '  FAIL  %s: %s (expected %s)\n' "$1" "$r" "$6"; fails=$((fails+1)); fi
}

can "platform reads the personas"       "access-guardrail-platform" list   clusterroles  ""           yes
can "platform reads the grants"         "access-guardrail-platform" list   rolebindings  ""           yes
can "platform grants per customer"      "access-guardrail-platform" create rolebindings  $ACCESS_NS   yes
can "platform revokes per customer"     "access-guardrail-platform" delete rolebindings  $ACCESS_NS   yes
# The cluster-scoped one is the grant that reaches every customer. The console
# refuses to make it; RBAC has to refuse it too, or the console's refusal is a
# suggestion that anybody can bypass with curl.
can "platform cannot grant cluster-wide" "access-guardrail-platform" create clusterrolebindings "" no
can "support reads no personas"         "access-guardrail-support"  list   clusterroles  ""           no
can "support hands out nothing"         "access-guardrail-support"  create rolebindings  $ACCESS_NS   no

# The escalation check, which is the whole point of naming the personas in `bind`.
#
# Granting cluster-admin must fail even though platform can create RoleBindings,
# because platform does not hold cluster-admin's permissions and cluster-admin is
# not in its list of bindable roles. If this ever passes, the bind rule has been
# widened into a way for anybody with the platform persona to make themselves
# cluster administrator.
grant_as() { # label, group, clusterrole, ok|denied
  out=$(kubectl create rolebinding "escalation-probe-$3" -n $ACCESS_NS \
    --clusterrole="$3" --group=whoever \
    --as=access-guardrail-probe --as-group="$2" 2>&1)
  kubectl delete rolebinding "escalation-probe-$3" -n $ACCESS_NS >/dev/null 2>&1
  if printf '%s' "$out" | grep -q 'created'; then r=ok
  elif printf '%s' "$out" | grep -qi 'forbidden\|not allowed to grant\|escalate'; then r=denied
  else r="another error entirely"; fi
  if [ "$4" = "$r" ]; then printf '  ok    %s\n' "$1"
  else printf '  FAIL  %s: %s (expected %s)\n         %s\n' "$1" "$r" "$4" "$out"; fails=$((fails+1)); fi
}

grant_as "platform may hand out a doblura persona" "access-guardrail-platform" "$QA"           ok
grant_as "platform may NOT hand out cluster-admin" "access-guardrail-platform" "cluster-admin" denied

# Every persona carries the label the console lists it by, and a summary. Without
# the label a persona is invisible on the page; without the summary the page shows
# a role name and nothing about what it does.
for p in customer viewer support qa consultancy platform; do
  name=$(role_for "$p")
  sum=$(kubectl get clusterrole "$name" \
    -o jsonpath='{.metadata.annotations.doblura\.dev/summary}' 2>/dev/null)
  if [ -n "$name" ] && [ -n "$sum" ]; then printf '  ok    %s is listed and described\n' "$p"
  else printf '  FAIL  %s: label=%s summary=%s\n' "$p" "${name:-missing}" "${sum:-missing}"; fails=$((fails+1)); fi
done

# ── the customer's own screen ──
#
# The status page is the one screen in this console that might be opened by
# somebody outside the company, so what its persona can reach is a security
# boundary rather than a convenience. Bound to ONE namespace, deliberately: bound
# cluster-wide it would show every customer the state of every other customer's
# environments, and the page scopes by RBAC and nothing else.
CUST=$(role_for customer)
CNS=$ACCESS_NS
OTHER=access-guardrail-other
kubectl create namespace $OTHER >/dev/null 2>&1
kubectl -n $CNS create rolebinding cust --clusterrole="$CUST" \
  --group=access-guardrail-customer >/dev/null 2>&1

# ncan asks about a resource, and takes a subresource SEPARATELY.
#
# Because `kubectl auth can-i get pods/log` answers a different question than the
# one it looks like: it sends resource="pods/log" with an empty subresource, which
# no RBAC rule is written against, and it came back "yes" for a persona whose real
# log read is refused. `--subresource=log` is the form that matches how the request
# is actually authorized. This mattered: the wrong form made a security check pass
# for the wrong reason, on the one persona that might be a person outside the
# company.
ncan() { # label, verb, resource, namespace, yes|no, [subresource]
  sub=""
  # ${6:-} and not $6: this script runs under `set -u`, and an unset positional is
  # a fatal error that killed every check below this one.
  [ -n "${6:-}" ] && sub="--subresource=${6:-}"
  # shellcheck disable=SC2086
  out=$(kubectl auth can-i "$2" "$3" -n "$4" $sub \
    --as=access-guardrail-probe --as-group=access-guardrail-customer 2>&1 | tr -d '[:space:]')
  case "$out" in yes*) r=yes ;; *) r=no ;; esac
  if [ "$5" = "$r" ]; then printf '  ok    %s\n' "$1"
  else printf '  FAIL  %s: %s (expected %s)\n' "$1" "$r" "$5"; fails=$((fails+1)); fi
}

ncan "a customer sees their own environments"  list odooenvironments "$CNS"   yes
ncan "and the deployments behind them"         list deployments      "$CNS"   yes
ncan "but NOT another customer's"              list odooenvironments "$OTHER" no
# The three that would make this page a liability.
ncan "no logs: they carry live data"           get  pods             "$CNS"   no log
ncan "no secrets"                              get  secrets          "$CNS"   no
ncan "no backups"                              list odoobackups      "$CNS"   no
# And nothing that changes anything. An environment restarted by the person
# reporting the problem, while somebody is looking into it, is worse than one
# nobody restarted.
ncan "cannot restart anything"                 patch odooenvironments "$CNS"  no
ncan "cannot delete anything"                  delete odooenvironments "$CNS" no

kubectl delete namespace $OTHER --ignore-not-found --wait=false >/dev/null 2>&1
kubectl delete namespace $ACCESS_NS --ignore-not-found --wait=false >/dev/null 2>&1
for p in platform support; do
  kubectl delete clusterrolebinding "access-guardrail-$p" --ignore-not-found >/dev/null 2>&1
done
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
