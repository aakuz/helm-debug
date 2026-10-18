# 0003. The tool never mutates the cluster

- Status: Accepted
- Date: 2026-10-18

## Context

`helm-debug` is used to investigate broken releases, often during incidents. The problems it finds
have known fixes: let Helm adopt an object by annotating and labeling it, or `helm rollback` /
`helm uninstall` for a stuck release. Running these automatically would be convenient but risky.

## Decision

The tool is strictly read-only. When it finds a problem it prints the `kubectl` or `helm` command
that fixes it, and never runs it.

Exit codes are `0` (no problems), `1` (execution error) and `2` (problems found). Code 2 is returned
after the full output has been printed.

## Consequences

- Only `get` and `list` RBAC is needed, so it is safe to give to on-call engineers.
- Exit code 2 makes it usable in CI checks.
- The user must copy and run the suggested fixes by hand. This costs a step, but it also gives a
  chance to review each change.
- Suggested commands are only hints. They may need adjusting, for example when `--overwrite` is
  not appropriate for the situation.
- Adding any mutating behavior later would require a new ADR that supersedes this one.
