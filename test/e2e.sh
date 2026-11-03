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
TMP=$(mktemp -d)
failures=0

cleanup() {
  [ "${KEEP_CLUSTER:-}" = 1 ] || kind delete cluster --name "$CLUSTER" >/dev/null 2>&1
  rm -rf "$KUBECONFIG" "$TMP"
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

# expect_out CODE PATTERN CMD... : like expect, and the output must also match the extended regexp PATTERN.
expect_out() {
  local want=$1 pattern=$2; shift 2
  local out code
  out=$("$@" 2>&1); code=$?
  if [ "$code" = "$want" ] && grep -qE -- "$pattern" <<<"$out"; then
    echo "ok   [$code] $* =~ /$pattern/"
  else
    echo "FAIL [$code, want $want, output must match /$pattern/] $*"; echo "$out" | sed 's/^/     /'
    failures=$((failures + 1))
  fi
}

# check DESCRIPTION CMD... : CMD must succeed.
check() {
  local desc=$1; shift
  if "$@"; then
    echo "ok   $desc"
  else
    echo "FAIL $desc"
    failures=$((failures + 1))
  fi
}

# mode FILE prints the permission bits in octal (GNU stat, then BSD stat).
mode() { stat -c %a "$1" 2>/dev/null || stat -f %Lp "$1"; }

kind get clusters | grep -qx "$CLUSTER" || kind create cluster --name "$CLUSTER" --wait 120s
kind export kubeconfig --name "$CLUSTER"
kubectl delete namespace "$NS" --ignore-not-found --wait
kubectl apply -f "$ROOT/test/e2e/crds/crds.yaml"
kubectl wait --for condition=established --timeout 60s crd/helmreleases.helm.toolkit.fluxcd.io crd/applications.argoproj.io

helm install demo "$CHART" -n "$NS" --create-namespace --wait --timeout 120s
helm install other "$CHART" -n "$NS" --wait --timeout 120s
helm install slow "$CHART" -n "$NS" --wait --timeout 120s
helm install drifty "$CHART" -n "$NS" --wait --timeout 120s
helm install hooked "$CHART" -n "$NS" --set hookJob=true --wait --timeout 120s
helm install legacy "$CHART" -n "$NS" --set legacyTestHook=true --wait --timeout 120s
helm install lost "$CHART" -n "$NS" --wait --timeout 120s
helm install crash "$CHART" -n "$NS" --set image=busybox:1.36 --set-json 'command=["sh","-c","echo boom; exit 1"]'

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

# Read-only views of healthy releases.
expect_out 0 'demo' "$BIN" list -n "$NS"
expect_out 0 'crash' "$BIN" list -A
expect 1 "$BIN" list -n "$NS" --storage bogus
expect 0 "$BIN" values demo -n "$NS"
expect_out 0 'image: registry.k8s.io/pause' "$BIN" values demo -n "$NS" --all
expect 0 "$BIN" values demo -n "$NS" --output-file "$TMP/values.yaml"
check "values --output-file has mode 600" test "$(mode "$TMP/values.yaml")" = 600
expect 1 "$BIN" values nope -n "$NS"
expect_out 0 'demo-hook' "$BIN" manifests demo -n "$NS" --hooks
expect 0 "$BIN" manifests demo -n "$NS" --hooks --output-dir "$TMP/manifests"
check "manifests --output-dir wrote the hook" grep -qr 'name: demo-hook' "$TMP/manifests"
for f in "$TMP"/manifests/*.yaml; do
  check "manifests --output-dir $(basename "$f") has mode 600" test "$(mode "$f")" = 600
done
expect 1 "$BIN" manifests demo -n "$NS" --revision 99
expect 0 "$BIN" health demo -n "$NS"
expect 0 "$BIN" health demo -n "$NS" --wait --timeout 60s
expect 0 "$BIN" hooks demo -n "$NS"
expect 0 "$BIN" deprecated demo -n "$NS"
expect 0 "$BIN" deprecated demo -n "$NS" --target-version 1.25
expect 0 "$BIN" audit -n "$NS"
expect 0 "$BIN" lost-found -n "$NS"
expect 0 "$BIN" drift drifty -n "$NS"

# Removed apiVersion in a stored test hook that was never applied.
expect_out 2 'policy/v1beta1' "$BIN" deprecated legacy -n "$NS"
expect_out 2 'removed' "$BIN" deprecated legacy -n "$NS" --target-version 1.25

# Live edit of a declared field.
kubectl -n "$NS" patch configmap drifty-config --type merge -p '{"data":{"key":"edited"}}'
expect_out 2 'edited' "$BIN" drift drifty -n "$NS"

# Leftover hook Job: Helm deleted it after success, recreate it by hand.
expect 0 "$BIN" hooks hooked -n "$NS"
kubectl -n "$NS" create job hooked-job --image=busybox:1.36 -- true
expect_out 2 'orphan' "$BIN" hooks hooked -n "$NS"

# Crashing pod: its log lines come through the pods/log subresource.
restarted() { [ "$(kubectl -n "$NS" get pod -l app=crash -o jsonpath='{.items[0].status.containerStatuses[0].restartCount}')" -ge 1 ] 2>/dev/null; }
for _ in $(seq 60); do restarted && break; sleep 2; done
check "crash pod restarted" restarted
expect_out 2 'boom' "$BIN" health crash -n "$NS" --logs 5
expect 2 "$BIN" health crash -n "$NS" --wait --timeout 5s --interval 1s

# Missing object and broken ownership.
kubectl -n "$NS" delete configmap demo-config
expect 2 "$BIN" resources demo -n "$NS"
expect 0 "$BIN" find configmap/demo-config -n "$NS" --all-revisions
kubectl -n "$NS" annotate deployment other meta.helm.sh/release-name=demo --overwrite
expect 2 "$BIN" resources other -n "$NS"

# Failed upgrade: the image never becomes ready.
helm upgrade demo "$CHART" -n "$NS" --set image=registry.invalid/nope:1 --wait --timeout 5s
expect 2 "$BIN" history demo -n "$NS"
expect_out 0 'registry.invalid/nope:1' "$BIN" diff demo 1 2 -n "$NS"
expect 1 "$BIN" diff demo 1 99 -n "$NS"
expect 2 "$BIN" audit -n "$NS" --max-revisions 1

# Flux HelmRelease and ArgoCD Application objects written by hand against stand-in CRDs.
kubectl -n "$NS" apply -f - <<EOF
apiVersion: helm.toolkit.fluxcd.io/v2
kind: HelmRelease
metadata: {name: other}
spec: {chart: {spec: {chart: demo}}}
status:
  conditions:
    - {type: Ready, status: "True", reason: UpgradeSucceeded, message: ok, lastTransitionTime: "2026-01-01T00:00:00Z"}
---
# Points at a healthy release, so only the Ready condition makes it fail.
apiVersion: helm.toolkit.fluxcd.io/v2
kind: HelmRelease
metadata: {name: broken}
spec: {releaseName: drifty, chart: {spec: {chart: demo}}}
status:
  conditions:
    - {type: Ready, status: "False", reason: UpgradeFailed, message: context deadline exceeded, lastTransitionTime: "2026-01-01T00:00:00Z"}
---
apiVersion: argoproj.io/v1alpha1
kind: Application
metadata: {name: other}
spec: {source: {repoURL: https://example.invalid/charts, chart: demo, targetRevision: 0.1.0}}
status:
  sync: {status: Synced, revision: 0.1.0}
  health: {status: Healthy}
  resources:
    - {version: v1, kind: ConfigMap, namespace: $NS, name: other-config, status: Synced}
---
apiVersion: argoproj.io/v1alpha1
kind: Application
metadata: {name: demo}
spec: {source: {repoURL: https://example.invalid/charts, chart: demo, targetRevision: 0.1.0}}
status:
  sync: {status: OutOfSync, revision: 0.1.0}
  health: {status: Missing}
  resources:
    - {version: v1, kind: ConfigMap, namespace: $NS, name: demo-config, status: OutOfSync, health: {status: Missing}}
EOF
expect_out 0 'UpgradeSucceeded' "$BIN" flux status other -n "$NS"
expect_out 2 'UpgradeFailed' "$BIN" flux status broken -n "$NS"
expect 1 "$BIN" flux status nope -n "$NS"
expect_out 0 'other-config' "$BIN" argo resources other -n "$NS"
expect_out 2 'demo-config' "$BIN" argo resources demo -n "$NS"
expect 1 "$BIN" argo resources nope -n "$NS"

# Release records deleted while its objects stay.
kubectl -n "$NS" delete secret -l owner=helm,name=lost
expect_out 2 'lost-config' "$BIN" lost-found -n "$NS"

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
expect_out 0 'pending-upgrade' "$BIN" list -n "$NS" --all
check "list hides the pending release without --all" sh -c "! '$BIN' list -n '$NS' | grep -q pending-upgrade"
"$BIN" stuck -A --older-than 0s

[ "$failures" = 0 ] && echo "e2e: all checks passed" || { echo "e2e: $failures check(s) failed"; exit 1; }
