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
compared in order and by length. No values are included in output, so Secret values stay private.
Unknown kinds and failed GETs return execution errors rather than an in-sync verdict.

## Diff

`diff RELEASE REV1 REV2` loads both revisions with `Store.Revision` and compares them without
touching live objects. Manifest documents are keyed by apiVersion/kind/namespace/name exactly as
stored (no scope resolution) and reported as `added`, `removed` or `changed` with a unified-style
line diff of the object YAML (sorted keys, 3 lines of context; a small built-in LCS diff, since no
diff library is in go.mod). Hooks are excluded, as in `drift`. Secret `data`/`stringData` values are
masked in both revisions before diffing: `<masked>` normally, `<masked:old>`/`<masked:new>` for a
key whose value differs, so a change is visible without printing anything or exposing a hash.
User values (`Release.Config`) are diffed as stored YAML, unmasked. A difference is information,
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

## Invocation modes

One `main`, built twice by goreleaser as `helm-debug` and `kubectl-helm_debug`, plus a Helm plugin
tarball that bundles `helm-debug` with a legacy-format `plugin.yaml` (ADR-0001). Help text uses
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
