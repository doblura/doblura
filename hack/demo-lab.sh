#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-or-later
# Copyright (C) 2026 Antoni Romera
#
# The demo lab: a cluster with doblura, the console, and customers in it.
#
# Why this exists, said plainly because the lesson cost a day: the console was
# built against a cluster assembled by hand — Traefik with its plugins, the
# customers, the environments, the accounts, the grants — and when that cluster
# was deleted, the only record of what had been in it was a paragraph of prose in
# a note outside the repository. hack/screenshots.sh still asks for CONSOLE= and
# a populated console, and nothing in this repository created one. A script that
# describes what another thing will provide, with nothing providing it, is the
# defect this project spent a day removing from its own code; it had no business
# staying in hack/.
#
# k3d rather than kind, unlike hack/e2e/run.sh, and the reason is not taste:
#   - k3s ships Traefik, which is the ingress the edge code writes middlewares
#     for. Without it spec.exposure produces objects nothing reads.
#   - host.k3d.internal resolves to the same address inside the cluster and out,
#     which is what hack/oidc-demo.sh needs to point a browser and a discovery
#     call at one issuer URL.
#
#   ./hack/demo-lab.sh up      cluster, operator, console, customers, accounts
#   ./hack/demo-lab.sh check   assert each account sees what it should
#   ./hack/demo-lab.sh open    port-forward the console and print the accounts
#   ./hack/demo-lab.sh scale   add 40 more customers, to measure the big case
#   ./hack/demo-lab.sh down    delete the cluster
#
# What it deliberately does NOT do: run a real migration end to end. That is
# hack/e2e/run.sh, it takes twenty minutes, and it needs a real Odoo image with a
# real database. This lab is about the interface and the edge.
set -euo pipefail

CLUSTER=${CLUSTER:-doblura}
NS=${NS:-doblura-system}
RELEASE=${RELEASE:-doblura}
PORT=${PORT:-8092}
IMAGE=${IMAGE:-doblura:dev}
ODOO_IMAGE=${ODOO_IMAGE:-doblura/odoo:18.0}
HERE="$(cd "$(dirname "$0")" && pwd)"
ROOT="$(cd "$HERE/.." && pwd)"

# The demo accounts. Local accounts and not an identity provider: this lab has to
# come up with nothing else running, and hack/oidc-demo.sh is what points the same
# console at a real issuer afterwards.
#
# One password for all three because it is a demo on a laptop and three passwords
# to remember is how a demo turns into a lookup. They are in a public repository,
# which is the same as saying the accounts are worth nothing outside this cluster.
DEMO_PASSWORD=${DEMO_PASSWORD:-demo-doblura-1234}

# EVERY call is pinned to this cluster's context, and none of them depends on
# which context happens to be current.
#
# This is not tidiness. The kubeconfig on a machine that does this work also holds
# contexts for real clusters, the current one can change between two commands for
# reasons that have nothing to do with this script, and a `kubectl delete` that
# picks up whatever is current is a script with a live production cluster inside
# its blast radius. It happened here — one command in this session ran against the
# wrong cluster, harmlessly, and there is no version of that which is worth
# risking twice.
CTX="k3d-$CLUSTER"
kc()  { kubectl --context "$CTX" "$@"; }
hlm() { helm --kube-context "$CTX" "$@"; }

say() { printf '\n\033[1m== %s\033[0m\n' "$*"; }
note() { printf '   %s\n' "$*"; }

need() {
  command -v "$1" >/dev/null 2>&1 || { echo "missing: $1" >&2; exit 1; }
}

up() {
  for t in k3d kubectl helm docker; do need "$t"; done

  say "1/7  cluster"
  if k3d cluster list -o json 2>/dev/null | grep -q "\"name\":\"$CLUSTER\""; then
    note "$CLUSTER already exists; starting it"
    k3d cluster start "$CLUSTER" >/dev/null
  else
    # One agent. Two was what this lab ran on originally and the node spent the
    # whole session a hair from out-of-memory; the work that needs a second node
    # is the federation demo, which is a second CLUSTER rather than a second node.
    #
    # 8080 on the host reaches Traefik, so the public environment's Ingress can be
    # curled from outside without a port-forward — which is the only way to see
    # the middlewares actually refuse something.
    k3d cluster create "$CLUSTER" --agents 1 \
      -p "8080:80@loadbalancer" -p "8443:443@loadbalancer" --wait
  fi
  kubectl config get-contexts "$CTX" >/dev/null 2>&1 || {
    echo "no kubeconfig context named $CTX" >&2; exit 1; }

  say "2/7  images"
  docker build -q -t "$IMAGE" "$ROOT" >/dev/null
  k3d image import -c "$CLUSTER" "$IMAGE" >/dev/null
  # The Odoo image is imported if it is on the machine and skipped if it is not.
  # Skipping is not a silent degradation: the environments still reconcile, their
  # pods pull and fail, and the console shows exactly that. Building it here would
  # add a multi-gigabyte build to a script whose job is to be quick.
  if docker image inspect "$ODOO_IMAGE" >/dev/null 2>&1; then
    k3d image import -c "$CLUSTER" "$ODOO_IMAGE" >/dev/null
    note "imported $ODOO_IMAGE"
  else
    note "$ODOO_IMAGE is not on this machine: environments will not boot."
    note "build it with 'make images' if you want them to."
  fi

  say "3/7  accounts"
  kc create namespace "$NS" --dry-run=client -o yaml | kc apply -f - >/dev/null
  # The hash comes out of the image that is about to run, through the same
  # subcommand the documentation tells an operator to use. Hashing with anything
  # else here would leave `console hash` untested by the only script that uses it.
  hash=$(printf '%s' "$DEMO_PASSWORD" \
    | docker run --rm -i --entrypoint /console "$IMAGE" hash --stdin)
  kc -n "$NS" create secret generic doblura-console-users \
    --from-literal=toni="$hash:doblura-platform" \
    --from-literal=ana="$hash:doblura-viewer,doblura-support,doblura-consultancy" \
    --from-literal=cliente-acme="$hash:cliente-acme" \
    --dry-run=client -o yaml | kc apply -f - >/dev/null
  # Stable across restarts on purpose: a session key regenerated on every rollout
  # signs everybody out whenever the console is redeployed, which during a day of
  # work reads as "the login is broken".
  kc -n "$NS" get secret doblura-console-session >/dev/null 2>&1 || \
    kc -n "$NS" create secret generic doblura-console-session \
      --from-literal=sessionKey="$(head -c 32 /dev/urandom | base64)" >/dev/null

  say "4/7  operator and console"
  hlm upgrade --install "$RELEASE" "$ROOT/charts/doblura" -n "$NS" --create-namespace \
    --set image.repository="${IMAGE%:*}" --set image.tag="${IMAGE#*:}" \
    --set image.pullPolicy=Never \
    --set console.enabled=true \
    --set console.localAccounts.secretName=doblura-console-users \
    --set console.sessionKeySecret=doblura-console-session \
    --wait --timeout 5m >/dev/null
  kc -n "$NS" rollout status deploy/"$RELEASE" --timeout=3m >/dev/null
  kc -n "$NS" rollout status deploy/"$RELEASE"-console --timeout=3m >/dev/null

  say "5/7  customers"
  # Postgres FIRST, and waited for. The other order is what this script did on
  # its first run: the environments were admitted, their init Jobs ran against a
  # database whose pod was still being created, and every environment in the lab
  # came up Failed. The operator was right and the lab was wrong, which is the
  # worst way round for something meant to demonstrate the operator.
  for ns in demo norte; do
    kc create namespace "$ns" --dry-run=client -o yaml | kc apply -f - >/dev/null
  done
  kc apply -f "$HERE/demo/postgres.yaml" >/dev/null
  kc -n demo rollout status deploy/pg --timeout=3m >/dev/null
  kc -n norte rollout status deploy/pg --timeout=3m >/dev/null
  # The production database, before the environment that expects to find one.
  kc -n demo delete job seed-production --ignore-not-found >/dev/null
  kc apply -f "$HERE/demo/seed-production.yaml" >/dev/null
  note "initialising the production database (a minute or two)"
  kc -n demo wait --for=condition=complete job/seed-production --timeout=10m >/dev/null \
    || { kc -n demo logs job/seed-production --tail=30; exit 1; }
  kc apply -f "$HERE/demo/customers.yaml" >/dev/null
  kc apply -f "$HERE/demo/snapshot.yaml" >/dev/null
  # Waited for, or `check` races it on a cold start and reports a pipeline that
  # is simply still running as one that did not run.
  note "taking the anonymised copy"
  for _ in $(seq 1 40); do
    phase=$(kc -n demo get odoosnapshot prod-anon -o jsonpath='{.status.phase}' 2>/dev/null)
    [ "$phase" = "Succeeded" ] || [ "$phase" = "Failed" ] && break
    sleep 5
  done
  [ "${phase:-}" = "Succeeded" ] || note "the snapshot is $phase; ./hack/demo-lab.sh check will say more"

  say "6/7  who may see what"
  # toni: the whole platform, cluster-wide.
  kc create clusterrolebinding demo-platform \
    --clusterrole="$RELEASE-platform" --group=doblura-platform \
    --dry-run=client -o yaml | kc apply -f - >/dev/null
  # ana: bound to ONE customer's namespace, which is the documented way to scope a
  # consultant and the case the console's customer scope exists for. She is not a
  # cluster-wide viewer here deliberately: with a cluster-wide binding every list
  # works and the scoping code is never exercised.
  for role in viewer support consultancy; do
    kc -n demo create rolebinding "demo-$role" \
      --clusterrole="$RELEASE-$role" --group="doblura-$role" \
      --dry-run=client -o yaml | kc apply -f - >/dev/null
  done
  # The customer, in their own namespace and nowhere else. Bound cluster-wide this
  # persona would show every customer the state of every other one.
  kc -n demo create rolebinding demo-customer \
    --clusterrole="$RELEASE-customer" --group=cliente-acme \
    --dry-run=client -o yaml | kc apply -f - >/dev/null

  say "7/7  ready"
  kc get odootenants -A 2>/dev/null || true
  echo
  note "console:  ./hack/demo-lab.sh open"
  note "check:    ./hack/demo-lab.sh check"
}

open_console() {
  echo "toni / $DEMO_PASSWORD          platform, every customer"
  echo "ana / $DEMO_PASSWORD           viewer+support+consultancy, only 'demo'"
  echo "cliente-acme / $DEMO_PASSWORD  the customer's own status page"
  echo
  echo "http://localhost:$PORT"
  kc -n "$NS" port-forward svc/"$RELEASE"-console "$PORT":80
}

# ── check ──
fails=0
ok()  { printf '  ok    %s\n' "$1"; }
bad() { printf '  FAIL  %s: %s\n' "$1" "$2"; fails=$((fails+1)); }

# Signs in as one of the demo accounts and leaves the session in a cookie jar.
# Through the login form and not a header, because that is the path a person
# takes and it is the one that can break.
signin() { # user, jar
  rm -f "$2"
  # The field is `user`, which is what internal/console/localauth.go reads. A
  # form posted with the wrong field name gets exactly the same answer as a wrong
  # password, so this is worth copying rather than guessing.
  curl -s -c "$2" -b "$2" -o /dev/null \
    -d "user=$1" -d "password=$DEMO_PASSWORD" "$BASE/auth/login"
}
page() { curl -s -b "$2" "$BASE$1"; }

check() {
  BASE=${CONSOLE:-http://localhost:$PORT}
  curl -sf -o /dev/null "$BASE/auth/login" || {
    echo "no console at $BASE — run '$0 open' in another terminal" >&2; exit 1; }

  local jar=/tmp/doblura-demo-jar

  # ── the platform account ──
  signin toni "$jar"
  grep -q doblura_session "$jar" && ok "toni signs in with a local account" \
    || { bad "toni signs in" "no session cookie"; return 1; }

  local envs; envs=$(page /o/environments "$jar")
  case "$envs" in
    *pr-482*) ok "platform sees environments across every customer" ;;
    *) bad "platform sees every customer's environments" "pr-482 is not listed" ;;
  esac
  case "$envs" in
    *staging*) ok "including the customer they were not looking at" ;;
    *) bad "norte's environments are listed too" "staging is not there" ;;
  esac

  # ── the consultant, bound to one namespace ──
  #
  # This is the case that was broken for a whole day: a list with no namespace is
  # a CLUSTER-scoped read, a RoleBinding never permits it, and the console used to
  # answer with an empty page. It must ask which customer instead.
  signin ana "$jar"
  # BOTH pages, and not just the front one. The "which customer?" answer was
  # wired into the overview alone, so anybody who clicked Environments in the
  # rail — which is most people — met "your groups do not permit reading these"
  # and went to ask for access they already had.
  for path in / /o/environments; do
    case "$(page "$path" "$jar")" in
      *"Which customer?"*)
        ok "a namespace-scoped person is asked which customer at $path" ;;
      *"do not permit"*)
        bad "ana is asked which customer at $path" \
          "she was refused instead: that sends her to ask for access she has" ;;
      *) bad "ana is asked which customer at $path" "the page answered neither" ;;
    esac
  done

  curl -s -c "$jar" -b "$jar" -o /dev/null -X POST "$BASE/scope" \
    -d 'to=demo&back=/o/environments'
  local scoped; scoped=$(page /o/environments "$jar")
  case "$scoped" in
    *pr-482*) ok "with a customer chosen, she sees that customer's environments" ;;
    *) bad "ana sees demo's environments once scoped" "pr-482 is not listed" ;;
  esac
  case "$scoped" in
    *staging*) bad "ana does not see the other customer" \
                 "norte's staging is on her page — the scope is not scoping" ;;
    *) ok "and not the other customer's" ;;
  esac

  # ── the customer ──
  signin cliente-acme "$jar"
  local mine; mine=$(page /status/demo "$jar")
  case "$mine" in
    *prod*) ok "the customer reaches their own status page" ;;
    *) bad "the customer's status page" "their own namespace was refused" ;;
  esac
  # Refused for the other customer, AND with a refusing status code. This page is
  # the one a customer bookmarks, so it is also the one somebody points a monitor
  # at, and it used to render "we cannot tell you" with a 200.
  local theirs; theirs=$(curl -s -b "$jar" -o /dev/null -w '%{http_code}' "$BASE/status/norte")
  case "$theirs" in
    403) ok "and another customer's namespace is refused, with a 403" ;;
    200) bad "another customer's status page is refused" \
           "it renders the refusal but answers 200, which every monitor reads as fine" ;;
    *) bad "another customer's status page is refused" "got $theirs" ;;
  esac
  # The one read this persona must never have: logs carry live customer data.
  #
  # Not a pipeline: `kubectl auth can-i` exits 1 for "no", and under pipefail that
  # turns the correct answer into a failed check.
  local canlog
  canlog=$(kc auth can-i get pods --subresource=log -n demo \
    --as=nobody --as-group=cliente-acme 2>/dev/null || true)
  [ "$canlog" = "no" ] && ok "the customer cannot read logs" \
    || bad "the customer cannot read logs" "the API server says: $canlog"

  # ── the anonymization pipeline, which is invisible until it runs ──
  #
  # Not "does the object exist": whether a dump came out, and whether the object
  # says what was actually masked rather than what the spec asked for. The
  # difference between those two numbers is the whole reason this check is here.
  local snap
  snap=$(kc -n demo get odoosnapshot prod-anon \
    -o jsonpath='{.status.phase}|{.status.columnsMasked}|{.status.sizeBytes}' 2>/dev/null)
  case "$snap" in
    Succeeded\|0\|*) bad "the snapshot masked something" \
      "it succeeded and masked no columns, which is a copy of production with the names left on" ;;
    Succeeded\|*\|0) bad "the snapshot produced a dump" "it succeeded with a size of zero" ;;
    Succeeded\|*) ok "the anonymization pipeline produced a masked dump ($snap)" ;;
    Copying*|"") bad "the snapshot finished" "still $snap after the lab came up" ;;
    *) bad "the snapshot succeeded" "phase is $snap" ;;
  esac
  # And the object knows what it did NOT mask. That is a data-protection fact:
  # a column listed there has its real values in that dump.
  if [ -n "$(kc -n demo get odoosnapshot prod-anon -o jsonpath='{.status.notMasked}' 2>/dev/null)" ]; then
    ok "and it records the rules it could not apply"
  else
    bad "the snapshot records what it did not mask" \
      "status.notMasked is empty; on this fixture five tables and one column are absent"
  fi
  signin toni "$jar"
  case "$(page /o/snapshots "$jar")" in
    *"not in this database"*) ok "and the console says so on the snapshots page" ;;
    *) bad "the console shows what was masked" \
         "the page does not mention the rules that could not be applied" ;;
  esac

  # ── the edge: the objects spec.exposure promises ──
  local mw
  mw=$(kc -n demo get middlewares.traefik.io -o name 2>/dev/null | wc -l | tr -d ' ')
  [ "${mw:-0}" -gt 0 ] && ok "the public environment's Traefik middlewares exist ($mw)" \
    || bad "middlewares exist for the public environment" \
         "none in demo — the Ingress would name middlewares nothing created"

  echo
  if [ "$fails" -eq 0 ]; then echo "  the lab is what it says it is"; else
    echo "  $fails check(s) failed"; return 1; fi
}

scale() {
  # The measurement that decided a product question: with 40 customers and ~200
  # environments every page still answered in under a second, which is why the
  # cache of decision 5 has not been built. It lived in a note and nothing
  # reproduced it, so it was one refactor away from quietly stopping being true.
  say "generating 40 customers"
  # The ephemeral-environment quota has to be raised first, and it is worth being
  # explicit about what that means: this is the safety limit that stops one person
  # filling the cluster, and the fixture is one person creating two hundred
  # environments. Refusing them is the webhook working. It is raised HERE and not
  # in `up`, so the lab a person actually looks at has the real limit.
  #
  # The key is webhook.maxEnvironmentsPerCreator and not maxEnvironmentsPerCreator:
  # the top-level name renders nothing, --set accepts it in silence, and the
  # refusals then look like the raise did not work.
  hlm upgrade "$RELEASE" "$ROOT/charts/doblura" -n "$NS" --reuse-values \
    --set webhook.maxEnvironmentsPerCreator=500 --wait --timeout 4m >/dev/null
  kc -n "$NS" rollout status deploy/"$RELEASE" --timeout=2m >/dev/null
  note "quota raised to 500 per creator for the fixture; 'up' puts it back"
  kc create namespace escala --dry-run=client -o yaml | kc apply -f - >/dev/null
  python3 "$HERE/demo/scale.py" | kc apply -f - >/dev/null
  kc get odooenvironments -n escala --no-headers | wc -l | xargs printf '   %s environments\n'
  echo
  # The measurement, taken rather than described. The number this produces is the
  # reason decision 5's cache has not been built, and a number that nothing
  # reproduces stops being true without anybody finding out.
  local jar=/tmp/doblura-demo-jar
  BASE=${CONSOLE:-http://localhost:$PORT}
  if curl -sf -o /dev/null "$BASE/auth/login" 2>/dev/null; then
    signin toni "$jar"
    local overview_size=0
    for path in / /o/environments /customers; do
      printf '   %-18s ' "$path"
      # Captured and then printed, rather than piped through tee: this runs in
      # CI and in terminals without a controlling tty, where /dev/tty does not
      # exist and the whole measurement dies on the first page.
      local measured size
      measured=$(curl -s -b "$jar" -o /tmp/doblura-scale-page \
        -w '%{time_total}s  %{size_download} bytes' "$BASE$path")
      echo "$measured"
      size=$(printf '%s' "$measured" | awk '{print $2}')
      [ "$path" = "/" ] && overview_size=${size:-0}
    done
    note "under a second each is what says the cache is still not needed"

    # And an assertion, not just a number printed for somebody to look at.
    #
    # The overview is a SUMMARY: it counts states, and its size must not follow
    # the number of environments. It once did — it enumerated all 215 of them,
    # 127KB of cards, and the fix to that is a page that cannot be told apart
    # from the broken one by reading the code. Only a page rendered against a lot
    # of data can tell, and until now nothing rendered one.
    echo
    if [ "${overview_size:-0}" -lt 40000 ]; then
      printf '  ok    the overview is a summary (%s bytes with %s environments)\n' \
        "$overview_size" "$(kc get odooenvironments -A --no-headers | wc -l | tr -d ' ')"
    else
      printf '  FAIL  the overview is %s bytes: it is enumerating environments\n' "$overview_size"
      printf '        rather than counting them, which is the regression this fixture exists to catch\n'
      return 1
    fi
  else
    note "no console at $BASE — run '$0 open' and then '$0 scale' again to time it"
  fi
}

down() {
  k3d cluster delete "$CLUSTER"
  docker rm -f doblura-dex >/dev/null 2>&1 || true
  echo "gone."
}

case "${1:-}" in
  up) up ;;
  check) check ;;
  open) open_console ;;
  scale) scale ;;
  down) down ;;
  *) echo "usage: $0 {up|check|open|scale|down}" >&2; exit 2 ;;
esac
