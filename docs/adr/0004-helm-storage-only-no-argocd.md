# 0004. Only releases with Helm storage records (no ArgoCD)

- Status: Superseded by [0005](0005-argocd-via-application-cr.md)
- Date: 2026-10-18

## Context

`helm-debug` works by reading release records (`sh.helm.release.v1.*` Secrets or ConfigMaps). ArgoCD
renders charts with `helm template` and applies the result, so it creates no release records.
Flux helm-controller and the Helm CLI do create them.

## Decision

Support only releases that have Helm storage records: those installed by the Helm CLI or Flux
helm-controller. ArgoCD-managed applications are out of scope. They should be debugged with Argo
tooling.

## Consequences

- The tool stays simple, with a single source of truth (the release records).
- Users of ArgoCD-managed apps get nothing from the tool. The README states this up front to avoid
  confusion.
- Running `helm-debug` on an ArgoCD app fails with "release not found", which can look like a bug
  unless the user has read the docs.
- Supporting ArgoCD would need a different mechanism, for example reading Argo Application
  resources, and would be a separate decision.
