#!/usr/bin/env bash
# Phase 0b: a real rehearsal, end to end.
#
# Everything before this verified form: that the API server rejects what it
# should, that YAML renders, that pure functions return what is expected. This
# script is the first time the pipeline actually runs against a real Odoo.
set -euo pipefail

CLUSTER=${CLUSTER:-doblura-real}
NS=doblura-e2e
ODOO_IMAGE=${ODOO_IMAGE:-doblura-odoo-test:19.0}
ODOO_VERSION=${ODOO_VERSION:-19.0}
HERE="$(cd "$(dirname "$0")" && pwd)"
ROOT="$(cd "$HERE/../.." && pwd)"

say() { printf '\n\033[1m== %s\033[0m\n' "$*"; }

say "1/8  fixture image ($ODOO_IMAGE)"
if ! docker image inspect "$ODOO_IMAGE" >/dev/null 2>&1; then
  docker build -f "$HERE/Dockerfile.odoo" --build-arg "ODOO_VERSION=$ODOO_VERSION" \
    -t "$ODOO_IMAGE" "$ROOT"
fi
docker run --rm -v "$ROOT/hack/verify-image.sh:/v.sh:ro" --entrypoint sh "$ODOO_IMAGE" /v.sh \
  | sed -n '/^CORE/,/^$/p;$p'

say "2/8  manager image"
docker build -q -t doblura:dev "$ROOT" >/dev/null

say "3/8  cluster"
kind create cluster --name "$CLUSTER" --wait 90s 2>/dev/null || true
kind export kubeconfig --name "$CLUSTER"
kind load docker-image --name "$CLUSTER" "$ODOO_IMAGE" doblura:dev

say "4/8  operator"
helm upgrade --install doblura "$ROOT/charts/doblura" -n doblura-system --create-namespace \
  --set image.repository=doblura --set image.tag=dev --set image.pullPolicy=Never \
  --wait --timeout 3m
kubectl -n doblura-system rollout status deploy/doblura --timeout=2m

say "5/8  postgres and dump volume"
kubectl create namespace "$NS" --dry-run=client -o yaml | kubectl apply -f -
kubectl apply -f "$HERE/manifests/postgres.yaml" -f "$HERE/manifests/dump-pvc.yaml"
kubectl -n "$NS" rollout status deploy/pg --timeout=3m

say "6/8  seed: initialise a real Odoo database and back it up"
kubectl -n "$NS" delete job seed --ignore-not-found
sed "s|doblura-odoo-test:19.0|$ODOO_IMAGE|g" "$HERE/manifests/seed-job.yaml" | kubectl apply -f -
if ! kubectl -n "$NS" wait --for=condition=complete job/seed --timeout=20m; then
  echo "--- seed logs ---"; kubectl -n "$NS" logs job/seed --tail=60; exit 1
fi
kubectl -n "$NS" logs job/seed --tail=6

say "7/8  the rehearsal"
kubectl -n "$NS" delete odoorehearsal e2e-odoo-19 --ignore-not-found
sed "s|doblura-odoo-test:19.0|$ODOO_IMAGE|g" "$HERE/manifests/rehearsal.yaml" | kubectl apply -f -

deadline=$(( $(date +%s) + 1500 ))
phase=""
while [ "$(date +%s)" -lt "$deadline" ]; do
  phase=$(kubectl -n "$NS" get odoorehearsal e2e-odoo-19 -o jsonpath='{.status.phase}' 2>/dev/null || true)
  case "$phase" in
    Succeeded|Failed) break ;;
  esac
  printf '  phase=%-12s %s\n' "${phase:-<none>}" "$(kubectl -n "$NS" get jobs -o custom-columns=:metadata.name --no-headers 2>/dev/null | tr '\n' ' ')"
  sleep 15
done

say "8/8  result"
kubectl -n "$NS" get odoorehearsal e2e-odoo-19
echo
kubectl -n "$NS" get odoorehearsal e2e-odoo-19 -o jsonpath='{range .status.conditions[*]}  {.type}={.status}  {.message}{"\n"}{end}'
echo
echo "  message:  $(kubectl -n "$NS" get odoorehearsal e2e-odoo-19 -o jsonpath='{.status.message}')"
echo "  duration: $(kubectl -n "$NS" get odoorehearsal e2e-odoo-19 -o jsonpath='{.status.upgradeDuration}')"
echo "  database: $(kubectl -n "$NS" get odoorehearsal e2e-odoo-19 -o jsonpath='{.status.databaseName}')"

if [ "$phase" != "Succeeded" ]; then
  echo
  echo "--- rehearsal did not succeed; job logs ---"
  for j in $(kubectl -n "$NS" get jobs -o custom-columns=:metadata.name --no-headers | grep e2e-odoo || true); do
    echo "### $j"; kubectl -n "$NS" logs "job/$j" --tail=40 2>&1 | sed 's/^/    /'
  done
  echo "--- operator logs ---"
  kubectl -n doblura-system logs deploy/doblura --tail=40 2>&1 | sed 's/^/    /'
  exit 1
fi
echo
echo "  PHASE 0B PASSED: a real rehearsal ran end to end against Odoo $ODOO_VERSION"
