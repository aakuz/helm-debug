# Design

## Scope

`helm-debug` answers questions about Helm releases from the records Helm keeps in the cluster:
what a release declares, what actually exists, who owns an object, what happened in past revisions
and which releases are locked. It never changes the cluster (ADR-0003). It only reads releases
that have storage records (ADR-0004).

## Layout

```
cmd/helm-debug        main: maps errors to exit codes
internal/cli          cobra commands, flags, env defaults, output (table/json/yaml)
internal/helm         release storage access and manifest parsing (Helm SDK v4)
internal/kube         kind resolution, live object lookups, ownership checks
test/e2e.sh           kind + real helm releases
```

Commands get their clients from `Options.NewClients`, which tests replace with fakes: the client-go
fake clientset for storage, the metadata fake for listing, the dynamic fake for live objects, and a
static REST mapper.

## Reading releases

- **Decoding** is done by the Helm SDK storage drivers (`driver.NewSecrets` / `driver.NewConfigMaps`),
  so every format Helm 3 and Helm 4 write is supported without custom code (ADR-0002).
- **History of one release** uses `driver.Query(name=<release>, owner=helm)` in the release namespace.
- **Cluster-wide scans** (`find`, `stuck`) avoid loading every record into memory at once:
  1. List record *metadata* only (`metadata.k8s.io` PartialObjectMetadata, `owner=helm`, pages of
     500), which gives release name and revision from labels without the `release` payload.
  2. Reduce to the latest revision per release, unless `--all-revisions` is set.
  3. Fetch and decode the remaining records in parallel (`--concurrency`) through the driver's `Get`.

  Records that fail to decode are counted and reported. They don't abort the scan.

## Objects of a release

The manifest is split with `releaseutil.SplitManifests`, and hooks come from `release.Hooks`. Each
document is reduced to apiVersion/kind/namespace/name. The REST mapper decides the scope: a
namespaced object with no namespace gets the release namespace, and a cluster-scoped object drops
any namespace. Kinds the cluster doesn't know (removed CRDs) are reported as `unknown-kind`.

## Drift

`drift` reads the latest release manifest and fetches each declared object, excluding hooks.
It reports missing objects and sorted JSON pointers to declared fields that changed or disappeared.
The comparison ignores status and server-managed metadata, and converts Secret stringData to data.
It recursively compares declared map keys, ignoring live-only keys so API defaults and controller
additions do not create noise. ConfigMap/Secret payload maps are compared in full so added data
keys are detected; other live-only additions are not detected. Known built-in resource quantity
fields use Kubernetes quantity comparison to avoid false drift from canonicalization. Lists are
compared in order and by length. Unknown kinds and failed GETs return execution errors rather than
an in-sync verdict.

For every drifted object `drift` also prints a unified diff (the same `unifiedDiff`/`paintDiff` as
`diff`, `--- Kind ns/name (status)` blocks after the status table; a `diff` string per row in
json/yaml). `kube.Project` builds it from the same normalization as `ChangedPaths`: the live object
is projected onto the declared structure (live-only keys dropped, except in ConfigMap/Secret payload
maps; equal-length lists projected element by element, other lists kept whole; quantities equal by
value take the declared string). `drift --values` instead prints the live values at the drifted
paths as YAML, one document per object, nested from the JSON pointers (a path inside a list yields
the whole live list, missing fields become `# removed:` comments). It does not map manifest fields
to chart values keys, since the manifest is already rendered; it is ignored for json/yaml output.
Values are shown, but Secret `data` values are masked (`<masked>`, `<masked:old>` for the declared
value, `<masked:new>` for the live one) unless the global `--show-secrets` flag is set. This
reveals more than the earlier path-only output, but still only reads (ADR-0003 is unaffected).
With `--show-secrets`, Secret values appear base64-encoded as stored (`stringData` is folded into `data`).
In `--values` mode, absent objects and lookup errors are emitted as `# absent:` / `# error:` comments.

## Diff

`diff RELEASE REV1 REV2` loads both revisions with `Store.Revision` and compares them without
touching live objects. Manifest documents are keyed by apiVersion/kind/namespace/name exactly as
stored (no scope resolution) and reported as `added`, `removed` or `changed` with a unified-style
line diff of the object YAML (sorted keys, 3 lines of context; a small built-in LCS diff, since no
diff library is in go.mod). Hooks are excluded, as in `drift`. Secret `data`/`stringData` values are
masked in both revisions before diffing: `<masked>` normally, `<masked:old>`/`<masked:new>` for a
key whose value differs, so a change is visible without printing anything or exposing a hash.
Masking is skipped with `--show-secrets`. User values (`Release.Config`) are diffed as stored
YAML, unmasked. A difference is information,
so the exit code is 0; only errors (unknown revision, bad arguments) exit 1.

## Ownership

Helm adopts and updates an existing object only if it has:

- `meta.helm.sh/release-name: <release>`
- `meta.helm.sh/release-namespace: <release namespace>`
- `app.kubernetes.io/managed-by: Helm`

`resources` reports `ok`, `missing` (no annotations) or `mismatch` (another release or wrong
label), and prints the `kubectl annotate`/`kubectl label` commands that let Helm adopt the object.
Hooks are not checked: Helm doesn't put this metadata on them.

`find` uses the same annotations as a fast path. It reads the live object, then verifies that the
annotated release really declares it, and falls back to a storage scan if it doesn't.

## Stuck releases

The latest revision of a release has status `pending-*`, and it became pending more than
`--older-than` ago. The suggested fix is `helm rollback <release> <last deployed/superseded revision>`,
or `helm uninstall` when no good revision exists (a failed first install).

## Health

Only `apps/v1` Deployments and StatefulSets of a revision are checked (hooks and other kinds are
ignored); they are fetched through fixed resources, so no REST mapping is needed. Rollout rules:

- Deployment: incomplete when `observedGeneration` is behind, `ProgressDeadlineExceeded` is set,
  updated replicas are below the desired count, old replicas still exist, or available replicas are
  below the desired count (the same checks as `kubectl rollout status`).
- StatefulSet: incomplete when `observedGeneration` is behind or ready replicas are below the desired
  count; updated replicas are compared against `replicas - partition`, and not at all for `OnDelete`.

The release status is part of the decision too: `pending-install`, `pending-upgrade`, `pending-rollback`,
`uninstalling` and `failed` are problems in both modes, even when the workloads look healthy, because an
operation may still be in progress or have failed, and an old Deployment can look healthy during an
upgrade. The table prints a `warning: release is <status>` line for them.

Pods come from the workload's label selector, so workloads with overlapping selectors report each
other's pods and old-ReplicaSet pods are included during a rollout. A pod is reported only when it
failed or a container waits with a known bad reason; unschedulable `Pending` pods and `minReadySeconds`
are only visible through the replica counts and events. Warning events of the last hour (a fixed
window) are matched by involved object: the workload, its pods, and ReplicaSets named
`<deployment>-<hash>`. Events never change the exit code; a lookup error for a workload does.

`--wait` runs the same snapshot as a single call on every poll and compares it with the previous one
(`healthSignature`), printing only when it differs. The state is compared as JSON without the events; the
events are compared by object, reason and message only, so a repeat of a known warning (higher count or
newer time) does not print. A new kind of warning, a status or count change, a restart count change or a
changed log line does (with `--tail` the signature is taken without logs, see below). The deadline is the
start time plus `--timeout`, read from the injectable clock (`Options.Now`), and pauses go through
`Options.Sleep`; the last pause is cut to the time left, so the final poll lands on the deadline. Only
table output streams; json and yaml print the final snapshot. Errors reading the release record abort the
wait; per-workload lookup errors leave the workload unhealthy and are retried on the next poll. Log
targets are picked per pod: a problem pod contributes its failing container (the one waiting with the
problem reason, else the first), read from the previous run when that container has restarted and is
currently waiting (`CrashLoopBackOff`); a running or just-exited container shows its current log. A
not-ready pod of an unhealthy workload contributes its first not-ready container. Containers that never
started are skipped. Log requests run one pod at a time per workload, so the number of simultaneous log
requests stays within `--concurrency`. Logs are read through the `pods/log` subresource (a GET, capped at
1 MiB per container), so the tool stays read-only (ADR-0003). `--logs` and `--all-logs` each accept 0 to
1000; anything else is an error. Lines are untrusted output: escape sequences (CSI, OSC, DCS/SOS/PM/APC
strings), control characters other than tab and Unicode format characters (bidi overrides, zero-width
characters) are removed, invalid UTF-8 is replaced, and lines are cut at 500 characters. The same
cleaning applies to log read error texts and to warning event reasons and messages.

Each log target carries its own line count (`logTarget.lines`), which keeps `--logs` and `--all-logs`
independent: the failing container of a pod that needs attention gets `--logs` lines (or `--all-logs` when
`--logs` is off), and with `--all-logs` every other started container of every pod, init containers included, gets
`--all-logs` lines.
Targets with zero lines are dropped.

`--tail` keeps, per log stream, the window of lines already printed (`logMemory`, keyed by namespace, pod,
container, previous-run flag, container ID and restart count). A restart changes the key, so the new run is a
new stream: its log is printed in full, under a heading without `new lines`, even when it reads exactly like
the crashed run before. Streams absent from a poll are forgotten (`logMemory.news`), so a pod that is deleted
or replaced, even by one with the same name, is shown in full again. The change signature is taken from the
snapshot without logs (`withoutLogs`), so a log change alone prints only the state line and the new lines.
`newLines` returns the lines after the longest suffix of the old window that is also a prefix of the new
window; when there is no overlap the whole new window is printed. Known limits: the match uses no timestamps,
so identical lines cannot be told apart, and lines that repeat the old window exactly (for example a repeated
heartbeat) can be hidden; when more lines arrive between two polls than the window holds (`--logs` or
`--all-logs` N), the lines in between are not shown. A read error is remembered per stream and printed again
only when its text changes.

## Flux HelmReleases

`flux status` reads the HelmRelease CR only to resolve the release name and storage namespace and to
show its conditions. The name is `spec.releaseName`, else `[targetNamespace-]name`, shortened when
longer than 53 characters; the storage namespace is `spec.storageNamespace`, else the HelmRelease
namespace. The history itself still comes from the release records, in the same format as `history`.
Exit code 2: the Ready condition is missing or not True, the latest revision is failed or pending,
or there are no release records.

## Invocation modes

One `main`, built twice by goreleaser as `helm-debug` and `kubectl-helm_debug`, plus a Helm plugin
tarball that bundles `helm-debug` with a legacy-format `plugin.yaml` (ADR-0001). The kubectl plugin is
also indexed in krew as `helm-debug`; krew-release-bot renders `.krew.yaml` on each release. Help text uses
the name the tool was invoked by:

- `kubectl-*` in `argv[0]` → `kubectl helm-debug`
- `HELM_PLUGIN_NAME` set → `helm debug`

Helm strips its global flags from plugin arguments and passes them as env vars. These are applied
as flag defaults in every mode, and explicit flags win:

| Env | Flag |
|---|---|
| `HELM_NAMESPACE` | `-n` |
| `HELM_KUBECONTEXT` | `--context` |
| `KUBECONFIG` | `--kubeconfig` |
| `HELM_KUBETOKEN` | `--token` |
| `HELM_KUBEAPISERVER` | `--server` |
| `HELM_KUBEASUSER` / `HELM_KUBEASGROUPS` | `--as` / `--as-group` |
| `HELM_KUBECAFILE` | `--certificate-authority` |
| `HELM_KUBEINSECURE_SKIP_TLS_VERIFY` | `--insecure-skip-tls-verify` |
| `HELM_KUBETLS_SERVER_NAME` | `--tls-server-name` |
| `HELM_DRIVER` | `--storage` |
| `HELM_DEBUG` | `--verbose` |

`--debug` is not used, because Helm consumes it as its own global flag.

## Output and exit codes

- `table` (default): colors only on a TTY and only when `NO_COLOR` is unset.
- `json`/`yaml`: the same result structs.

Exit codes: `0` ok, `1` execution error, `2` problems found. Code 2 is returned via `cli.ErrProblems`
after the full output has been printed.
