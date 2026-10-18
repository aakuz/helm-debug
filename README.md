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
| 2 | Problems found: absent object or broken ownership (`resources`), drift (`drift`), object not declared by any release (`find`), latest revision failed/pending (`history`), stuck releases (`stuck`) |

## RBAC

| Command | Needs |
|---|---|
| `resources` | `get`/`list` on secrets (or configmaps) in the release namespace, `get` on every declared object |
| `drift` | `get`/`list` on secrets (or configmaps) in the release namespace, `get` on every declared object |
| `diff` | `list` on secrets (or configmaps) in the release namespace |
| `history` | `list` on secrets (or configmaps) in the release namespace |
| `find` | `get` on the object; `list` + `get` on secrets (or configmaps) cluster-wide for the scan |
| `stuck` | `list` + `get` on secrets (or configmaps) in the namespace, or cluster-wide with `-A` |

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
