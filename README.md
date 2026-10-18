# helm-debug

Debug Helm releases by reading their records straight from the cluster storage
(the `sh.helm.release.v1.*` Secrets or ConfigMaps), without running Helm operations.

- **`resources`** – which objects a release revision declares (hooks included), whether they exist
  and whether they carry the ownership metadata Helm needs to manage them.
- **`drift`** – which declared fields differ between the latest stored manifest and live objects.
- **`find`** – which release(s) declare a given object.
- **`history`** – revisions with status, chart/app version and the stored failure reason.
- **`stuck`** – releases stuck in `pending-install/upgrade/rollback` and the command that unlocks them.

The tool is strictly read-only. When it finds a problem it prints the `kubectl`/`helm` command
that fixes it, but never runs it.

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
```

Compares the latest stored manifest with live objects using read-only GET requests. Missing
objects and changed or removed declared fields count as drift. Changed fields are reported as
JSON pointers; values are never printed, including Secret values. `-o json` and `-o yaml` expose
the same resource statuses and changed paths.

Status and server-managed metadata (such as `managedFields`, `resourceVersion`, UID and timestamps)
are ignored. Fields present only on live objects are ignored to avoid reporting API defaults and
controller additions. Consequently, additions outside the declared fields are not detected. Lists
are compared in order, including their length; list reordering can be reported as drift. Secret
`stringData` is compared against its stored `data` representation. Hooks are excluded because Helm
commonly deletes them after execution. This compares stored intent, not a newly rendered chart.

Exit code is 0 when in sync, 2 for drift, and 1 for lookup or manifest errors (including unknown
kinds). An execution error takes precedence over drift when both occur.

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

## Exit codes

| Code | Meaning |
|---|---|
| 0 | No problems |
| 1 | Execution error (no access, release not found, bad flags, …) |
| 2 | Problems found: absent object or broken ownership (`resources`), drift (`drift`), object not declared by any release (`find`), latest revision failed/pending (`history`), stuck releases (`stuck`) |

## RBAC

| Command | Needs |
|---|---|
| `resources` | `get`/`list` on secrets (or configmaps) in the release namespace, `get` on every declared object |
| `drift` | `get`/`list` on secrets (or configmaps) in the release namespace, `get` on every declared object |
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

## License

MIT © 2025 Anton Kuznetsov
