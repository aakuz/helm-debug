<p align="center">
  <img src="logo.png" alt="helm-debug" width="600">
</p>

# helm-debug

Read-only CLI for debugging Helm releases. It reads release records straight from the cluster
storage (the `sh.helm.release.v1.*` Secrets or ConfigMaps) and compares them with live objects,
without running Helm operations and without changing anything in the cluster.

- **`resources`** – which objects a release revision declares (hooks included), whether they exist
  and whether they carry the ownership metadata Helm needs to manage them.
- **`drift`** – which declared fields differ between the latest stored manifest and live objects.
- **`diff`** – rendered manifest and user-values differences between two revisions.
- **`find`** – which release(s) declare a given object.
- **`history`** – revisions with status, chart/app version and the stored failure reason.
- **`stuck`** – releases stuck in `pending-install/upgrade/rollback` and the command that unlocks them.
- **`health`** – readiness of the release's Deployments and StatefulSets, crashing pods and recent warning events.
- **`lost-found`** – objects that claim a Helm release which no longer exists in the cluster.
- **`hooks`** – hooks of a release revision (event, weight, delete policy, last run) and leftover hook Jobs and Pods.
- **`audit`** – revision counts and record sizes per release, and releases whose objects are all gone from the cluster.
- **`values`** – user-supplied or computed values of a release revision, as a values file.

The tool is strictly read-only. When it finds a problem it prints the `kubectl`/`helm` command
that fixes it, but never runs it. Output is a table by default, or `-o json|yaml` for scripts
and CI; table output is highlighted in a terminal (see [Output and colors](#output-and-colors)).

## Quick start

```bash
helm-debug stuck -A                    # which releases are locked, and how to unlock them
helm-debug history backend -n prod     # what happened to a release, and why the last upgrade failed
helm-debug diff backend 1 2 -n prod    # what changed between two revisions
helm-debug resources backend -n prod   # do the declared objects exist, and does Helm own them
helm-debug drift backend -n prod       # did someone edit the objects by hand
helm-debug health backend -n prod      # are the workloads rolled out and are pods crashing
helm-debug find deploy/backend -A      # which release owns this object
```

It works on Linux, macOS and Windows and can be run three ways:

| Invocation | Binary |
|---|---|
| `helm-debug …` | `helm-debug` |
| `kubectl helm-debug …` | `kubectl-helm_debug` on `PATH` |
| `helm debug …` | Helm plugin |

> Releases deployed by ArgoCD are rendered with `helm template` and have no release records,
> so there is nothing for this tool to read. It targets releases installed by the Helm CLI or
> Flux helm-controller.

## Install

### Binaries

Download the archive for your platform from the
[latest release](https://github.com/aakuz/helm-debug/releases/latest). It contains both
`helm-debug` and `kubectl-helm_debug` (`.exe` on Windows). Put them on your `PATH`.

From source (Go 1.27+):

```bash
go install github.com/aakuz/helm-debug/cmd/helm-debug@latest
```

To also use it as a kubectl plugin, copy or link the same binary as `kubectl-helm_debug`.

### Helm plugin

Pick the tarball for your OS and architecture (`linux|darwin|windows` × `amd64|arm64`):

```bash
# Helm 4 (plugins are not signed yet, so verification has to be skipped)
helm plugin install --verify=false \
  https://github.com/aakuz/helm-debug/releases/latest/download/helm-debug-plugin-darwin-arm64.tgz

# Helm 3
helm plugin install \
  https://github.com/aakuz/helm-debug/releases/latest/download/helm-debug-plugin-linux-amd64.tgz
```

Remove with `helm plugin uninstall debug`.

## Usage

All commands accept the usual kubectl flags (`--context`, `--kubeconfig`, `-n`, …) plus:

| Flag | Meaning |
|---|---|
| `-o, --output table\|json\|yaml` | Output format (default `table`) |
| `--storage secret\|configmap` | Helm storage driver (default `$HELM_DRIVER`, else `secret`) |
| `--concurrency N` | Parallel API requests (default 8) |
| `-v, --verbose` | Debug logging to stderr (also `HELM_DEBUG=true`) |
| `--show-secrets` | Show Secret `data` values in `drift` and `diff` instead of masking them (default off) |

When run as `helm debug`, Helm passes its global flags (`-n`, `--kube-context`, `--kubeconfig`,
`--kube-token`, …) through `HELM_*` environment variables. `helm-debug` picks them up as defaults
in every mode, and explicit flags take precedence.

### resources

```console
$ helm-debug resources backend -n prod
Release prod/backend revision 2 (deployed)

KIND         NAMESPACE  NAME             HOOK         STATUS        OWNERSHIP
ClusterRole  -          backend-reader   -            present       ok
ConfigMap    shared     backend-config   -            present       mismatch (prod/frontend managed-by=Helm)
Deployment   prod       backend          -            present       ok
Service      prod       backend          -            present       missing
Widget       -          backend-widget   -            unknown-kind  -
Job          prod       backend-migrate  pre-upgrade  absent        -

error: Widget/backend-widget: no matches for kind "Widget" in version "example.com/v1"

Suggested fixes (let Helm adopt the objects):
  kubectl -n shared annotate ConfigMap/backend-config meta.helm.sh/release-name=backend meta.helm.sh/release-namespace=prod --overwrite
  …
```

`--revision N` inspects an older revision. Hooks are listed, but their absence is not treated as a
problem because hooks are usually deleted by their delete policy.

### drift

```console
$ helm-debug drift backend -n prod
Release prod/backend revision 2

KIND        NAMESPACE  NAME     STATUS   CHANGED FIELDS
Deployment  prod       backend  drifted  /spec/replicas

--- Deployment prod/backend (drifted)
@@ -4,4 +4,4 @@
   name: backend
   namespace: prod
 spec:
-  replicas: 2
+  replicas: 3
```

Compares the latest stored manifest with live objects using read-only GET requests. Missing
objects and changed or removed declared fields count as drift. Changed fields are reported as
JSON pointers, and each drifted object is followed by a unified diff of the declared (expected)
fields against the live ones, built with the same rules as the comparison below (live-only fields
do not show up). Secret `data` values are masked as in `diff` (`<masked>`, `<masked:old>` for the
declared value, `<masked:new>` for the live one) unless `--show-secrets` is set. `-o json` and
`-o yaml` expose the resource statuses, changed paths and a `diff` string for each drifted row.

`drift --values` prints, instead of the table, a values-like YAML of the live values at the drifted
paths: one document per drifted object, separated by `---` and preceded by a `# Kind namespace/name`
comment, with a `# removed: /path` comment for fields missing from the live object. A drifted path
inside a list includes the whole live list. Secret values print as `<masked>` unless
`--show-secrets` is set, in which case they appear base64-encoded as stored (`stringData` is folded
into `data`). Absent objects and lookup errors appear as `# absent:` / `# error:` comments. The
output is not mapped to chart values keys: the manifest is already rendered, so you map the fields
to your values by hand. The flag is ignored with `-o json` or `-o yaml`, which already carry the
data. The exit code is unchanged.

Status and server-managed metadata (such as `managedFields`, `resourceVersion`, UID and timestamps)
are ignored. Fields present only on live objects are ignored to avoid reporting API defaults and
controller additions. ConfigMap/Secret payload maps (`data` and ConfigMap `binaryData`) are
compared in full, including added keys. Other additions outside the declared fields are not
detected. Kubernetes resource quantities in standard workload and storage fields are compared by
value (for example, `100m` CPU equals `0.1`). Lists are compared in order, including their length;
list reordering can be reported as drift. Secret
`stringData` is compared against its stored `data` representation. Hooks are excluded because Helm
commonly deletes them after execution. This compares stored intent, not a newly rendered chart.

Exit code is 0 when in sync, 2 for drift, and 1 for lookup or manifest errors (including unknown
kinds). An execution error takes precedence over drift when both occur.

### diff

```console
$ helm-debug diff backend 1 2 -n prod
Release prod/backend revisions 1 -> 2

Manifest:
KIND        NAMESPACE  NAME     STATUS
Deployment  -          backend  changed

--- Deployment backend (changed)
@@ -3,4 +3,4 @@
 metadata:
   name: backend
 spec:
-  replicas: 2
+  replicas: 3

Values:
@@ -1,2 +1,2 @@
 image: backend
-replicas: 2
+replicas: 3
```

Compares the stored manifests and the user-supplied values (`helm get values`) of two revisions.
Objects are matched by apiVersion, kind, namespace and name and reported as `added`, `removed` or
`changed`, with a unified-style diff of each object's YAML. Hooks are excluded, as in `drift`.
Secret `data` and `stringData` values are masked unless `--show-secrets` is set: unchanged values
print as `<masked>`, and a value that changed prints as `<masked:old>` and `<masked:new>`. User
values are shown as stored, so do not keep secrets in them. `-o json` and `-o yaml` return the objects with their status and diff text,
and the values diff. Only storage records are read, never live objects.

Exit code is 0 whether or not the revisions differ; unknown revisions and other errors exit 1.

### find

```console
$ helm-debug find deployment/backend -n prod
Live object prod/Deployment/backend: present, annotated with release prod/backend

RELEASE NAMESPACE  RELEASE  REVISION  STATUS    UPDATED              OBJECT NAMESPACE  HOOK
prod               backend  2         deployed  2026-10-18 04:00:00  prod              -
```

The command first reads the `meta.helm.sh/release-*` annotations of the live object and checks
that the annotated release really declares it. If that gives no answer, it scans release records
in all namespaces: object metadata is listed page by page, and only the latest revision of each
release is decoded, unless you pass `--all-revisions`. `KIND` accepts kinds, plurals and short
names (`deploy`, `svc`, `cm`). `-A` matches the object in any namespace.

### history

```console
$ helm-debug history backend -n prod
REVISION  UPDATED              STATUS      CHART          APP VERSION  DESCRIPTION
3         2026-10-18 05:00:00  failed      backend-1.3.0  2.0          Upgrade "backend" failed: timed out waiting for the condition
2         2026-10-18 04:00:00  deployed    backend-1.2.0  2.0          -
```

### stuck

```console
$ helm-debug stuck -A
NAMESPACE  RELEASE  REVISION  STATUS           AGE  FIX
dev        fresh    1         pending-install  9h   helm uninstall fresh -n dev
prod       backend  3         pending-upgrade  7h   helm rollback backend 2 -n prod
```

Releases that became pending less than `--older-than` ago (default `5m`) are skipped, because they
may still be in progress.

### lost-found

```console
$ helm-debug lost-found -A
RELEASE NAMESPACE  RELEASE  KIND         NAMESPACE  NAME
dev                orphan   ConfigMap    dev        orphan-cfg
prod               gone     ClusterRole  -          gone-reader
prod               gone     Deployment   prod       gone
```

Matches every object labeled `app.kubernetes.io/managed-by=Helm` and annotated with
`meta.helm.sh/release-name` against the release records of all namespaces. Namespaced kinds
are checked in the current namespace, or everywhere with `-A`; cluster-scoped objects (ClusterRole, …)
are attributed to the namespace of the release they claim and shown only for that namespace. Objects
without the release annotation are skipped, since the release they belong to cannot be named.
Kinds that cannot be listed (for example for lack of permissions) are reported as warnings.

The scan makes one request per resource kind, so in a terminal it shows a `Scanning N/M resource kinds`
status line on stderr. The line is erased when the scan ends and is not printed when stderr is
redirected.

### hooks

```console
$ helm-debug hooks backend -n prod
Release prod/backend revision 2

NAME             KIND  EVENT        WEIGHT  DELETE-POLICY   PHASE      LAST RUN  STATUS
backend-migrate  Job   pre-upgrade  5       hook-succeeded  Succeeded  1h ago    orphan

warning: 1 hook object(s) outlived their delete policy
```

`--revision N` inspects an older revision. A hook Job or Pod is an orphan when it still exists although
its last run finished and its delete policy (`hook-succeeded` or `hook-failed`) asks for removal after
that outcome. Objects kept by policy or by Helm's default `before-hook-creation` are not orphans.

### values

```console
$ helm-debug values backend -n prod --revision 2 --output-file values.yaml
```

Prints the user-supplied values of a revision (default latest) as plain YAML, ready for
`helm upgrade -f` or `helm template -f`. `--all` prints the computed values, the chart defaults
merged with the user values. `-o json` prints JSON and `--output-file PATH` writes the values to a
file (mode 0600) instead of stdout. Values may contain secrets, so a warning is printed to stderr;
they are never logged, not even with `--verbose`.

### deprecated

```console
$ helm-debug deprecated backend -n prod --target-version 1.32
Release prod/backend revision 3, target Kubernetes 1.32

KIND     NAME     APIVERSION          HOOK  STATUS   REMOVED-IN  REPLACEMENT
Ingress  backend  extensions/v1beta1        removed  1.22        networking.k8s.io/v1
```

Scans the manifest and hooks of a revision (`--revision N`, default latest) for apiVersions removed in the
target version, like helm-mapkubeapis but read-only. The target defaults to the cluster's server version, so
pass `--target-version` to check an upgrade before it happens. Statuses: `removed` (gone at the target
version, per a built-in table of the Kubernetes deprecation guide), `deprecated` (still served at the
target version) and `not-served` (a built-in apiVersion the connected cluster no longer serves, whether or
not it is in the table; kinds from CRD groups are not checked against the cluster).

### audit

```console
$ helm-debug audit -A
NAMESPACE  RELEASE  STATUS    REVISIONS  LARGEST   TOTAL     FINDINGS
dev        scratch  deployed  1          3.1KiB    3.1KiB    orphaned
prod       backend  deployed  34         812.0KiB  9.8MiB    revisions,record-size
prod       web      deployed  4          40.2KiB   120.5KiB
```

Lists every release (`-A` for all namespaces) with its revision count and the size of its release
records. A record counts towards the 1 MiB Kubernetes object limit, so a release is flagged when
it has more than `--max-revisions` revisions (default `20`), when its largest record exceeds
`--max-record-size` (default `800Ki`), or when it is `orphaned`: its latest revision declares objects
and none of them exist. Hooks are ignored, and releases that are pending (an operation may still be creating their objects) or
uninstalled with kept history are never orphaned. A lookup that fails (for example for lack of permissions, or a kind the cluster does not know)
does not prove the objects are gone, so it never flags a release.

Record sizes come from listing the full records in pages of 50, so `-A` on a big cluster transfers
all release data once.

### health

```console
$ helm-debug health web -n prod
Release prod/web revision 1 (deployed)

KIND         NAMESPACE  NAME  READY  STATUS     DETAILS
Deployment   prod       web   1/3    unhealthy  1 of 3 available
StatefulSet  prod       db    1/2    unhealthy  1 of 2 ready

Pods with problems:
POD      PHASE    REASON            RESTARTS
web-aaa  Running  CrashLoopBackOff  7
web-bbb  Pending  ImagePullBackOff  0

Warning events (last hour):
LAST SEEN  OBJECT               REASON        COUNT  MESSAGE
5m         ReplicaSet/web-5d8f  FailedCreate  1      exceeded quota
10m        Pod/web-aaa          BackOff       12     Back-off restarting failed container
```

Checks the Deployments and StatefulSets declared by a release revision; hooks and other kinds are
ignored. For a Deployment, the rollout is incomplete when the controller has not observed the
current generation, replicas are not yet updated or available, old replicas are still pending
termination, or the rollout reports `ProgressDeadlineExceeded`. For a StatefulSet, it is incomplete
when the generation is not observed, replicas are not ready, or replicas below the
`rollingUpdate.partition` are not updated (the update check is skipped for `OnDelete`).

Pods are found through the workload's label selector and listed under "Pods with problems" only
when they failed, or a container is waiting with `CrashLoopBackOff`, `ImagePullBackOff`,
`ErrImagePull`, `InvalidImageName`, `CreateContainerConfigError`, `CreateContainerError` or
`RunContainerError`. Restarts are summed over init and regular containers. Warning events from the
last hour are shown for the workloads, their ReplicaSets and their pods.

`--revision N` (`-r`) inspects an older revision. `-o json` and `-o yaml` return the same data,
including the events. The command is read-only: it only GETs the workloads, LISTs pods and events
in their namespaces, and reads the release records like the other commands.

Exit code is 0 when the release status is not a problem status, every workload is rolled out and no pod
is listed. It is 2 when the release status is `pending-install`, `pending-upgrade`, `pending-rollback`,
`uninstalling` or `failed` (its workloads may not reflect the latest revision yet, so the table prints
a `warning: release is <status>` line), when a workload is not fully rolled out, absent or could not be
read (status `error`), or when a pod with problems is listed. Warning events alone do not cause 2. Errors
that stop the command (for example, no such release or no access to the release records) exit 1.

Within one snapshot the output comes in this order: the release line, the workloads table, the pods with
problems, the warning events and, last, the logs, which are the longest part.

`--logs N` (default `0`, off) adds the last N log lines of the containers that explain a problem, and
`--all-logs M` (default `0`, off) adds the last M lines of every started container of every pod, init
containers included, healthy pods included. N and M must be between 0 and 1000, otherwise the command
fails. The two are independent and can be combined: the container that explains a problem in a pod that
needs attention (listed under "Pods with problems", or not Ready in an unhealthy workload) gets N lines,
and every other started container gets M lines. With `--all-logs` alone, the pods that need attention get
M lines too; with `--logs` alone, only the pods that need attention are read. Logs are read through the
`pods/log` subresource, a read-only GET capped at 1 MiB per container, one pod at a time per workload, so
both flags need the same `get` on `pods/log`. A container that has restarted and is currently waiting
(for example `CrashLoopBackOff`) shows the log of its previous run; a container that is running or has
just exited shows its current log. Containers that never started (`ImagePullBackOff`, `ErrImagePull`,
`InvalidImageName`, `CreateContainerConfigError`) have no log and are skipped. Each log is printed under
`Logs of POD (container NAME[, previous run]):`, indented by two spaces. A failed log request prints
`warning: unavailable: <error>` and does not change the exit code. Log lines are untrusted container
output: terminal escape sequences (CSI, OSC, DCS, SOS, PM and APC strings) and control characters (except
tab) are removed, Unicode format characters (bidi overrides, zero-width characters) are removed, invalid
UTF-8 is replaced, and lines longer than 500 characters are cut with an ellipsis. The same cleaning
applies to log read error texts and to warning event reasons and messages. In `-o json` and `-o yaml` the
logs are a `logs` array per workload (`pod`, `container`, `previous`, `lines`, `error`), and problem pods
gain a `container` field.

`--wait` (`-w`) polls until the release is healthy: the release status is not a problem status, every
workload is rolled out and no pod is listed, which is the condition under which exit code 0 is returned
without it. `--interval` (default `2s`) is the time between polls and `--timeout` (default `5m`) the
total wait; both must be positive, and passing either without `--wait` is an error (`--timeout requires
--wait`). In table mode the state is printed on the first poll and again whenever it changes: a status or
count change, a restart count change, a new or changed problem pod, a new kind of warning event, or a
changed log line when `--logs` or `--all-logs` is set. A repeat of a known warning event (higher count or
newer time, same object, reason and message) does not print a new snapshot. Each print starts with a
`[HH:MM:SS +ELAPSED] healthy|unhealthy` line, and polls without a change print nothing. On success the
command exits 0. When the timeout passes first it prints `timed out after DURATION, the release is still
unhealthy` and exits 2. Ctrl-C stops the command like any other tool (the shell reports 130). Errors
reading the release record (unknown release, storage errors) abort the wait with exit 1; per-workload
lookup errors are retried until the timeout. With `-o json` or `-o yaml` nothing is streamed; only the
final snapshot is printed.

`--tail` (with `--wait`, and with `--logs` or `--all-logs`; otherwise the command fails with `--tail requires
--wait` or `--tail requires --logs or --all-logs`) follows the logs like `tail -f`. The first poll prints the
logs in full. Later polls print only the log lines that were not shown before, under headings such as
`Logs of POD (container NAME, previous run, new lines):`. A log stream is one run of one container of a pod,
identified by namespace, pod, container, previous-run flag, container ID and restart count. Only a stream that
continues a log already shown gets the `new lines` label. A restart starts a new stream, so its log is printed
in full under a heading without `new lines`, even when it reads exactly like the log of the crashed run before
(a deterministic crash loop). A stream that is absent from a poll (the pod was deleted or replaced) is
forgotten, so a pod that comes back, even with the same name, is shown in full again. A poll where only the
logs changed prints just the `[HH:MM:SS +ELAPSED] healthy|unhealthy` line and the new lines, without repeating
the table; a poll where the state changed prints the state without the logs, followed by the new lines; a poll
with nothing new prints nothing. Without `--tail` every update repeats the whole tail. New lines are found by
matching the end of the previously shown window with the start of the new one; when they do not overlap, the
whole new window is printed. Known limits: the match uses no timestamps, so identical lines cannot be told
apart, and lines that repeat the old window exactly (for example a repeated heartbeat) can be hidden; when more
lines arrive between two polls than the window holds (`--logs` or `--all-logs` N), the lines in between are not
shown. In tail mode a read error is shown once, and again only when its text changes. With `-o json` or
`-o yaml`, `--tail` has no effect: the final snapshot carries the full tail.

```console
$ helm-debug health backend -n prod --wait --logs 5
[12:00:00 +0s] unhealthy
Release prod/backend revision 2 (deployed)

KIND        NAMESPACE  NAME     READY  STATUS     DETAILS
Deployment  prod       backend  1/3    unhealthy  1 of 3 available

Pods with problems:
POD          PHASE    REASON            RESTARTS
backend-aaa  Running  CrashLoopBackOff  7

Logs of backend-aaa (container main, previous run):
  starting
  panic: no database

...

[12:00:04 +4s] healthy
Release prod/backend revision 2 (deployed)

KIND        NAMESPACE  NAME     READY  STATUS   DETAILS
Deployment  prod       backend  3/3    healthy  -
```

With `--tail` the first poll prints the logs in full, and each later poll prints only the new lines:

```console
$ helm-debug health backend -n prod -w --logs 10 --tail
[12:00:00 +0s] unhealthy
Release prod/backend revision 1 (deployed)

KIND        NAMESPACE  NAME     READY  STATUS     DETAILS
Deployment  prod       backend  1/3    unhealthy  1 of 3 available

Pods with problems:
POD          PHASE    REASON            RESTARTS
backend-aaa  Running  CrashLoopBackOff  7

Logs of backend-aaa (container main, previous run):
  starting
  panic: no database

[12:00:02 +2s] unhealthy

Logs of backend-aaa (container main, previous run, new lines):
  retry 1

...
```

## Output and colors

`table` output is meant for people; `-o json` and `-o yaml` carry the same data for scripts, with
no colors. In a terminal, `table` output is highlighted:

| Element | Color |
|---|---|
| Resource names in `diff` and `drift` | bold magenta |
| Healthy statuses (`deployed`, `present`, `ok`, `in-sync`, `added`) | green |
| Problem statuses (`failed`, `absent`, `missing`, `mismatch`, `unknown-kind`, `removed`) | red |
| In-progress or modified statuses (`pending-*`, `uninstalling`, `drifted`, `changed`) | yellow |
| Added / removed lines and `@@` hunk headers in diffs | green / red / cyan |

Colors are enabled only when stdout is a terminal and disabled when `NO_COLOR` is set.

## Configuration

| Variable | Effect |
|---|---|
| `HELM_DRIVER` | Default for `--storage` (`secret` or `configmap`) |
| `HELM_DEBUG=true` | Same as `--verbose` |
| `NO_COLOR` | Disable colored output |
| `HELM_NAMESPACE`, `HELM_KUBECONTEXT`, `HELM_KUBETOKEN`, `HELM_KUBEAPISERVER`, … | Defaults for the matching kubectl flags, set by Helm when run as `helm debug` |

## Exit codes

| Code | Meaning |
|---|---|
| 0 | No problems (`diff` always exits 0 when it succeeds) |
| 1 | Execution error (no access, release not found, bad flags, …) |
| 2 | Problems found: absent object or broken ownership (`resources`), drift (`drift`), object not declared by any release (`find`), latest revision failed/pending (`history`), stuck releases (`stuck`), lost resources (`lost-found`), removed or deprecated apiVersions (`deprecated`), orphaned hook objects (`hooks`), releases that need cleanup (`audit`) |

## RBAC

| Command | Needs |
|---|---|
| `resources` | `get`/`list` on secrets (or configmaps) in the release namespace, `get` on every declared object |
| `drift` | `get`/`list` on secrets (or configmaps) in the release namespace, `get` on every declared object |
| `diff` | `list` on secrets (or configmaps) in the release namespace |
| `history` | `list` on secrets (or configmaps) in the release namespace |
| `find` | `get` on the object; `list` + `get` on secrets (or configmaps) cluster-wide for the scan |
| `stuck` | `list` + `get` on secrets (or configmaps) in the namespace, or cluster-wide with `-A` |
| `lost-found` | `list` on secrets (or configmaps) cluster-wide, `list` on every kind (metadata only) in the namespace, or cluster-wide with `-A`, plus API discovery |
| `hooks` | `get`/`list` on secrets (or configmaps) in the release namespace, `get` on the hook Jobs and Pods |
| `deprecated` | `get`/`list` on secrets (or configmaps) in the release namespace, API discovery (and `get` on `/version` unless `--target-version` is given) |
| `audit` | `list` on secrets (or configmaps) in the namespace, or cluster-wide with `-A`, and `get` on the objects of every latest revision |
| `health` | `get`/`list` on secrets (or configmaps) in the release namespace, `get` on the Deployments and StatefulSets, `list` on pods and events in their namespaces, `get` on `pods/log` with `--logs` or `--all-logs` |

## Development

```bash
make build          # bin/helm-debug and bin/kubectl-helm_debug
make test           # unit tests with fake clients and golden files
make update-golden  # rewrite testdata/*.golden after intended output changes
make lint           # golangci-lint
make e2e            # kind cluster + real helm releases (needs docker, kind, kubectl, helm)
```

Design notes live in [docs/DESIGN.md](docs/DESIGN.md), decisions in [docs/adr](docs/adr).
Planned work is tracked in [issues labelled `roadmap`](https://github.com/aakuz/helm-debug/issues?q=label%3Aroadmap).

## Contributing

Open an issue to discuss a change, then send a pull request. Keep the tool read-only (ADR-0003),
write a failing test first, and make sure `make test`, `make lint` and `make e2e` pass.

## License

MIT © 2025 Anton Kuznetsov
