# 0005. ArgoCD support through the Application CR

- Status: Accepted
- Date: 2026-10-26
- Supersedes: [0004](0004-helm-storage-only-no-argocd.md)

## Context

`helm-debug` works by reading release records (`sh.helm.release.v1.*` Secrets or ConfigMaps). ArgoCD
renders charts with `helm template` and applies the result, so it creates no release records. Argo
applications were therefore invisible to every release-record command. Users asked for ArgoCD support
in #16.

## Decision

A separate `argo` command group reads the Application CR (`argoproj.io`) and checks each tracked object
live. It uses `spec.source` / `spec.sources`, `status.sync`, `status.health`, `status.operationState` and
`status.resources`.

Ownership is judged by ArgoCD tracking metadata, not by `meta.helm.sh` annotations: the annotation
`argocd.argoproj.io/tracking-id` if present, otherwise the label `app.kubernetes.io/instance`.

Release-record commands stay Helm-storage only. The tool stays read-only, as decided in ADR-0003.

## Consequences

- Argo users get a view of resources, sync, health and tracking for an Application (`argo resources`).
- It needs `get` on `applications.argoproj.io` in the Application namespace, besides `get` on each tracked object.
- When the Application CRD is absent from the cluster, the command says so instead of failing with a
  generic lookup error.
- Helm-specific commands (`history`, `drift`, `values`, ...) still do not apply to Argo apps, because
  ArgoCD keeps no release records.
