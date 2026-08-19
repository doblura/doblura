#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-or-later
# Copyright (C) 2026 Antoni Romera
#
# Single sign-on, against a real identity provider, on a laptop.
#
# The console has had OIDC since it was written and it had never been pointed at
# an issuer. "It compiles" is not evidence about a protocol with a redirect, a
# state cookie, a token exchange and a claim mapping — so this stands one up and
# runs the whole flow, including the parts that are supposed to fail.
#
# Dex, and on the HOST rather than in the cluster. The issuer URL has to resolve
# to the same thing from two places at once: the console does discovery from
# inside the cluster, and the browser is redirected to the same URL from outside
# it. A Service address fails the second, a port-forward fails the first.
# host.k3d.internal is the one name k3d makes true on both sides.
#
#   ./hack/oidc-demo.sh up      start dex and point the console at it
#   ./hack/oidc-demo.sh check   run the flow and assert what comes out
#   ./hack/oidc-demo.sh down    stop dex and put the console back
set -euo pipefail

NS=${NS:-doblura-system}
RELEASE=${RELEASE:-doblura}
CONSOLE_URL=${CONSOLE_URL:-http://localhost:8092}
ISSUER=http://host.k3d.internal:5556/dex
DEXDIR=${DEXDIR:-/tmp/doblura-dex}
RESOLVE=(--resolve host.k3d.internal:5556:127.0.0.1)

up() {
  mkdir -p "$DEXDIR"
  # Two connectors on purpose. mockCallback is the only one that emits GROUPS,
  # which is the claim doblura's whole authorization model rests on; the password
  # connector is there so the ordinary "type a password at your IdP" path exists
  # too. Testing only the password one would prove the redirect works and prove
  # nothing about groups.
  cat > "$DEXDIR/config.yaml" <<YAML
issuer: $ISSUER
storage: {type: memory}
web: {http: 0.0.0.0:5556}
oauth2: {skipApprovalScreen: true}
staticClients:
  - id: doblura-console
    name: Doblura console
    secret: consola-demo-secreto
    redirectURIs: ["$CONSOLE_URL/auth/callback"]
enablePasswordDB: true
staticPasswords:
  - email: "elena@ejemplo.test"
    hash: "\$2a\$12\$16aYlH.WHgQ0m/9M3lJ0ru5RtEHXQTPHu9OWZE5rgcaEkNJm8Vy6."
    username: "elena"
    userID: "1001"
connectors:
  - {type: mockCallback, id: mock, name: "Grupos de ejemplo"}
YAML

  docker rm -f doblura-dex >/dev/null 2>&1 || true
  docker run -d --name doblura-dex -p 5556:5556 \
    -v "$DEXDIR/config.yaml:/etc/dex/config.yaml:ro" \
    ghcr.io/dexidp/dex:v2.41.1 dex serve /etc/dex/config.yaml >/dev/null

  for _ in $(seq 1 30); do
    curl -sf "${RESOLVE[@]}" "$ISSUER/.well-known/openid-configuration" >/dev/null 2>&1 && break
    sleep 1
  done

  kubectl -n "$NS" create secret generic dex-client \
    --from-literal=clientSecret=consola-demo-secreto \
    --dry-run=client -o yaml | kubectl apply -f - >/dev/null

  helm upgrade "$RELEASE" ./charts/doblura -n "$NS" --reuse-values \
    --set console.oidc.issuer="$ISSUER" \
    --set console.oidc.clientID=doblura-console \
    --set console.oidc.redirectURL="$CONSOLE_URL/auth/callback" \
    --set console.oidc.groupsClaim=groups \
    --set console.oidc.clientSecretSecret=dex-client \
    --wait --timeout 4m >/dev/null
  kubectl -n "$NS" rollout status deploy/"$RELEASE"-console --timeout=150s >/dev/null

  echo "dex is up and the console is pointed at it."
  echo "Port-forward the console and run: $0 check"
}

fails=0
ok()   { printf '  ok    %s\n' "$1"; }
bad()  { printf '  FAIL  %s: %s\n' "$1" "$2"; fails=$((fails+1)); }

check() {
  local jar=/tmp/doblura-oidc-jar
  rm -f "$jar"

  # The console discovered the issuer at startup or it would not be offering SSO.
  if curl -s "$CONSOLE_URL/auth/login" | grep -q '/auth/sso'; then
    ok "the console discovered the issuer and offers single sign-on"
  else
    bad "the console offers single sign-on" "it does not; check its logs"
    return 1
  fi

  local loc
  loc=$(curl -s -c "$jar" -b "$jar" -o /dev/null -D - "$CONSOLE_URL/auth/sso" \
    | grep -i '^location:' | tr -d '\r' | cut -d' ' -f2-)
  case "$loc" in
    "$ISSUER"*) ok "it redirects to the identity provider" ;;
    *) bad "it redirects to the identity provider" "went to $loc"; return 1 ;;
  esac
  grep -q doblura_state "$jar" && ok "a state cookie is set for the flow" \
    || bad "a state cookie is set" "none was"

  # Straight to the connector that emits groups.
  curl -s "${RESOLVE[@]}" -c "$jar" -b "$jar" "$loc" -o /tmp/doblura-dex-page.html
  local mock
  mock=$(python3 - <<'PY'
import re, html
s = open('/tmp/doblura-dex-page.html').read()
m = [html.unescape(h) for h in re.findall(r'href="([^"]*)"', s) if '/auth/mock' in h]
print('http://host.k3d.internal:5556' + m[0] if m else '')
PY
)
  [ -n "$mock" ] || { bad "the provider offers the mock connector" "not on its page"; return 1; }

  local final
  final=$(curl -s -L "${RESOLVE[@]}" -c "$jar" -b "$jar" "$mock" -o /dev/null -w '%{url_effective}')
  case "$final" in
    "$CONSOLE_URL"*) ok "the whole flow lands back on the console" ;;
    *) bad "the flow lands back on the console" "ended at $final"; return 1 ;;
  esac
  grep -q doblura_session "$jar" && ok "a session was issued" \
    || { bad "a session was issued" "no session cookie"; return 1; }

  # The claims, which is the part that decides everything afterwards.
  curl -s -b "$jar" "$CONSOLE_URL/me" -o /tmp/doblura-me.html
  local who
  who=$(python3 - <<'PY'
import re, html
t = html.unescape(re.sub(r'\s+', ' ', re.sub(r'<[^>]+>', ' ', open('/tmp/doblura-me.html').read())))
print(t)
PY
)
  case "$who" in
    *"Kilgore Trout"*) ok "the name claim reached the console" ;;
    *) bad "the name claim reached the console" "it did not" ;;
  esac
  case "$who" in
    *authors*) ok "the GROUPS claim reached the console" ;;
    *) bad "the groups claim reached the console" \
        "no group from the provider is on the page — check --oidc-groups-claim" ;;
  esac

  # And that a group from the provider is what Kubernetes authorizes on. This is
  # the claim the whole design makes: change a binding, and what the person can do
  # changes, with no sign-out and nothing in doblura to update.
  kubectl -n demo delete rolebinding oidc-demo --ignore-not-found >/dev/null 2>&1

  # Scoped to the customer first, because the grant below is a RoleBinding to one
  # namespace and a list with no namespace is a CLUSTER-scoped read that such a
  # binding never permits. Without this the page is refused before and after, and
  # the check reports the grant as having done nothing — which is what happened
  # the first time this ran.
  curl -s -c "$jar" -b "$jar" -X POST "$CONSOLE_URL/scope" \
    -d 'to=demo&back=/o/environments' -o /dev/null

  local before after
  before=$(curl -s -b "$jar" "$CONSOLE_URL/o/environments" | grep -c 'do not permit' || true)
  kubectl -n demo create rolebinding oidc-demo \
    --clusterrole="$RELEASE-viewer" --group=authors >/dev/null
  sleep 2
  after=$(curl -s -b "$jar" "$CONSOLE_URL/o/environments" | grep -c 'do not permit' || true)
  kubectl -n demo delete rolebinding oidc-demo --ignore-not-found >/dev/null 2>&1

  if [ "$before" -gt 0 ] && [ "$after" -eq 0 ]; then
    ok "binding the provider's group changed what they can do, with no sign-out"
  else
    bad "binding the provider's group changes access" \
      "refused before=$before after=$after (expected some, then none)"
  fi

  # The state cookie is the CSRF defence on the flow itself. Without it somebody
  # can complete a sign-in in another person's browser as themselves.
  local code
  code=$(curl -s -o /dev/null -w '%{http_code}' \
    "$CONSOLE_URL/auth/callback?code=anything&state=invented")
  [ "$code" = "400" ] && ok "a callback with somebody else's state is refused" \
    || bad "a callback with a foreign state is refused" "got $code"

  echo
  [ "$fails" -eq 0 ] && echo "  single sign-on works end to end" \
    || { echo "  $fails check(s) failed"; return 1; }
}

down() {
  docker rm -f doblura-dex >/dev/null 2>&1 || true
  helm upgrade "$RELEASE" ./charts/doblura -n "$NS" --reuse-values \
    --set console.oidc.issuer= \
    --set console.oidc.clientID= \
    --set console.oidc.redirectURL= \
    --set console.oidc.clientSecretSecret= \
    --wait --timeout 4m >/dev/null
  kubectl -n "$NS" delete secret dex-client --ignore-not-found >/dev/null
  echo "dex stopped and the console put back on local accounts."
}

case "${1:-check}" in
  up) up ;;
  check) check ;;
  down) down ;;
  *) echo "usage: $0 {up|check|down}" >&2; exit 2 ;;
esac
