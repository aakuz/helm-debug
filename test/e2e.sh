#!/usr/bin/env bash
# End-to-end test against a kind cluster with real `helm install` releases.
# Requires docker, kind, kubectl and helm. KEEP_CLUSTER=1 leaves the cluster running.
set -uo pipefail

CLUSTER=helm-debug-e2e
NS=e2e
ROOT=$(cd "$(dirname "$0")/.." && pwd)
BIN=$ROOT/bin/helm-debug
CHART=$ROOT/test/e2e/chart
export KUBECONFIG=$(mktemp)
failures=0

cleanup() {
  [ "${KEEP_CLUSTER:-}" = 1 ] || kind delete cluster --name "$CLUSTER" >/dev/null 2>&1
  rm -f "$KUBECONFIG"
}
trap cleanup EXIT

# expect CODE CMD... : run CMD and check its exit code.
expect() {
  local want=$1; shift
  local out code
  out=$("$@" 2>&1); code=$?
  if [ "$code" = "$want" ]; then
    echo "ok   [$code] $*"
  else
    echo "FAIL [$code, want $want] $*"; echo "$out" | sed 's/^/     /'
    failures=$((failures + 1))
  fi
}

kind get clusters | grep -qx "$CLUSTER" || kind create cluster --name "$CLUSTER" --wait 120s
kind export kubeconfig --name "$CLUSTER"
kubectl delete namespace "$NS" --ignore-not-found --wait

helm install demo "$CHART" -n "$NS" --create-namespace --wait --timeout 120s
helm install other "$CHART" -n "$NS" --wait --timeout 120s
helm install slow "$CHART" -n "$NS" --wait --timeout 120s

expect 0 "$BIN" resources demo -n "$NS"
expect 0 "$BIN" history demo -n "$NS"
expect 0 "$BIN" find configmap/demo-config -n "$NS"
expect 0 "$BIN" find cm demo-hook -A
expect 2 "$BIN" find configmap/nope -n "$NS"
expect 1 "$BIN" history nope -n "$NS"

# Kubectl plugin and helm-style env defaults.
expect 0 env PATH="$ROOT/bin:$PATH" kubectl helm-debug history demo -n "$NS"
expect 0 env HELM_NAMESPACE="$NS" "$BIN" resources demo
expect 0 env HELM_DRIVER=secret "$BIN" stuck -A

# Missing object and broken ownership.
kubectl -n "$NS" delete configmap demo-config
expect 2 "$BIN" resources demo -n "$NS"
expect 0 "$BIN" find configmap/demo-config -n "$NS" --all-revisions
kubectl -n "$NS" annotate deployment other meta.helm.sh/release-name=demo --overwrite
expect 2 "$BIN" resources other -n "$NS"

# Failed upgrade: the image never becomes ready.
helm upgrade demo "$CHART" -n "$NS" --set image=registry.invalid/nope:1 --wait --timeout 5s
expect 2 "$BIN" history demo -n "$NS"

# Stuck upgrade: kill helm while it waits.
helm upgrade slow "$CHART" -n "$NS" --set image=registry.invalid/nope:2 --wait --timeout 120s &
pid=$!
for _ in $(seq 30); do
  kubectl -n "$NS" get secret -l name=slow,status=pending-upgrade -o name | grep -q . && break
  sleep 1
done
kill -9 "$pid"; wait "$pid" 2>/dev/null
expect 2 "$BIN" stuck -n "$NS" --older-than 0s
expect 0 "$BIN" stuck -n "$NS"
"$BIN" stuck -A --older-than 0s

[ "$failures" = 0 ] && echo "e2e: all checks passed" || { echo "e2e: $failures check(s) failed"; exit 1; }
