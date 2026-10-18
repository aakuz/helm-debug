# 0001. Go rewrite: one binary, three invocations

- Status: Accepted
- Date: 2026-10-18

## Context

The tool started as two Python scripts (`helm-resources-checker.py`, `helm-resource-finder.py`) that
decoded release records with hand-written base64/gzip/zlib/bz2 heuristics. It needs to run as a
standalone command, as a kubectl plugin and as a Helm plugin, on Linux, macOS and Windows.
kubectl discovers plugins by file name (`kubectl-helm_debug`), and symlinks are awkward on Windows.

## Decision

Rewrite in Go with a single `main`, built by goreleaser as two binaries, `helm-debug` and
`kubectl-helm_debug`, for linux/darwin/windows x amd64/arm64. Both binaries ship in each archive.

The Helm plugin (`helm debug`) is a per-OS/arch tarball that bundles `helm-debug` with a
legacy-format `plugin.yaml`. The legacy format works on Helm 3 and Helm 4, while the v1 format is
Helm-4-only. We will migrate when Helm 5 drops the legacy format.

Help text adapts to the invocation name (`kubectl-*` in `argv[0]`, or `HELM_PLUGIN_NAME`). Helm
strips its global flags from plugin arguments and passes them as `HELM_*` env vars, so these are
flag defaults in all modes, and explicit flags win. The Python scripts are removed.

## Consequences

- One code path and one set of tests for all three invocations.
- No symlinks needed, so installation works the same on Windows.
- Archives are roughly 2x larger because the same binary is included twice.
- Plugin tarballs are unsigned for now. Helm 4 verifies plugins by default, so users must pass
  `--verify=false`. Git installs cannot be verified at all.
- The legacy `plugin.yaml` format is a known migration debt for Helm 5.
- Anyone using the Python scripts must switch to the Go binary.
