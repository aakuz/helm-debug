# 0002. Use the Helm SDK v4 for release storage

- Status: Accepted
- Date: 2026-10-18

## Context

The Python scripts decoded `sh.helm.release.v1.*` records with their own heuristics (base64, gzip,
zlib, bz2). That is fragile and has to be kept in sync with whatever Helm 3 and Helm 4 write.

## Decision

Use the storage drivers and release types from `helm.sh/helm/v4` (`driver.NewSecrets`,
`driver.NewConfigMaps`, the `release/v1` types, `releaseutil.SplitManifests`) instead of our own decoder.

The drivers' `List()` loads every record, which is too heavy for cluster-wide scans. So `find` and
`stuck` list record metadata only (`metadata.k8s.io` PartialObjectMetadata, `owner=helm`, pages of
500), reduce to the latest revision per release, then decode the chosen records in parallel with the
driver's `Get`.

The SQL storage driver is not supported. It is rare and needs a DSN.

## Consequences

- Every format Helm 3 and Helm 4 write is decoded correctly, with no custom code to maintain.
- Secret and ConfigMap drivers come for free, along with hook and release types.
- The dependency tree is large and the binary weighs tens of MB (~76 MB unstripped).
- A cluster-wide scan costs N GET requests, one per chosen record, in addition to the metadata
  listing. `--concurrency` bounds the parallelism.
- Users of `HELM_DRIVER=sql` cannot use the tool.
- We follow the Helm SDK's API changes when upgrading.
