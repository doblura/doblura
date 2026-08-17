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

echo
[ $fails -eq 0 ] && echo "  all guardrails OK" || { echo "  $fails guardrail(s) failed"; exit 1; }
