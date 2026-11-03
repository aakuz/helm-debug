# 0006. Client-side chart rendering for lint

- Status: Accepted
- Date: 2026-11-03

## Context

Every command so far reads release records or live objects (ADR 0002, 0005). When a chart renders invalid
YAML, Helm names only the file and a line of the rendered output and does not show the output
(helm/helm#8982, #9684, #8655). A misspelled value that renders an empty `metadata.name` is reported later
with an unrelated error (helm/helm#8847). Users asked for a command that shows the rendered lines around the
error, in #43. That needs a chart, not a release record.

## Decision

`helm-debug lint CHART` renders a local chart (directory or `.tgz`) with the Helm SDK engine, the way
`helm template` does, and never contacts the cluster: `lookup` returns empty and `.Capabilities` are Helm's
defaults. The render step lives in `internal/render` so the upgrade diff (#17) can reuse it.

text/template stops at the first failing template. To report every problem in one run (helm/helm#11055),
when the chart does not render as a whole, each executable template is rendered on its own, with the others
renamed to partials so the blocks they define stay available.

Values come from local `-f` files and `--set` only. Repo and OCI chart references are left to #17.

The tool stays read-only (ADR 0003). Exit codes follow it: 2 for problems found, 1 for execution errors.

## Consequences

- Chart authors get the failing rendered lines without `helm template --debug` and counting lines.
- Templates that depend on `lookup` or the server version may render differently at install time.
- A failing chart is rendered once per template, which is slower for large charts but only on failure.
- Secret values in context lines are masked line by line, because the document being reported may not parse.
  A Secret whose `kind` line is itself broken is not recognized and is not masked.
- The SDK engine adds sprig and its dependencies to the binary.
