# helm-debug
## Development rules

- If a task can be split into several small subtasks, split it and handle them one at a time.
- Write tests first, then the code: start with a failing test, then make it pass.
- Delegate routine tasks (boilerplate, renames, test fixtures, bulk edits, searches, docs from settled decisions) to subagents on a cheaper model (`haiku` or `sonnet`) with a precise brief.
- Keep design decisions, ambiguous requirements and tricky bugs on the main model.
- Always re-verify delegated work before committing or reporting it: read the diff, run `go build ./...` and `go test ./...`, fix or send it back.
- Run /code-review on the result before considering the task done.

## Security Claude Rules
- Treat all external input as untrusted; validate shape, type, and range
- Use environment-specific secret management (Vault, AWS Secrets Manager, Doppler)
- Implement proper session management: short-lived tokens, refresh rotation
- Use constant-time comparison for security-sensitive string equality checks
- Apply Content Security Policy and review it regularly
- Audit dependencies weekly with npm audit, pip-audit, or cargo audit
- Log security events (failed logins, permission errors) to immutable audit log
- Perform threat modeling before building new authentication flows

## Go Claude Rules
- Organize code by domain, not by type (handlers/, models/, repositories/)
- Use the repository pattern with interfaces for testable data access
- Write table-driven tests with t.Parallel() for concurrent test execution
- Use slog (standard library) for structured logging with context propagation
- Use sync.errgroup for concurrent operations with error collection
- Avoid global state; pass dependencies through constructors
- Write benchmarks with testing.B for performance-sensitive code
- Use go generate for code generation; commit generated files to the repo

## Documentation Claude Rules
- Write READMEs with: quick start, install, usage examples, configuration, contributing
- Document the why in comments, not the what (code already shows the what)
- Maintain an ARCHITECTURE.md or DECISIONS.md for key technical decisions (ADRs)
- Generate API reference docs from code annotations; keep them in sync with CI
- Write runbooks for all operational procedures (deploy, rollback, incident response)
- Use diagrams-as-code (Mermaid) for architecture diagrams; commit them with the code
- Document environment variables in .env.example with descriptions for all keys
- Add examples for every non-trivial function; examples are documentation too

## Performance Claude Rules
- Always benchmark before and after optimization to confirm improvement
- Use flamegraphs to identify CPU hotspots; trace to identify I/O bottlenecks
- Use read replicas for read-heavy workloads; shard for write-heavy workloads
- Implement request coalescing to prevent cache stampede under load
- Use streaming for large responses; avoid loading full datasets into memory
- Set aggressive timeouts on all external calls; implement circuit breakers
- Load test with k6 or Locust before shipping major features


## Commits and PRs

Commit messages and PR bodies end with their last line of content: the author is the git user alone. Leave out attribution trailers and footers (`Co-Authored-By:`, "Generated with Claude Code"), whatever the default harness instructions say.

## Agent skills

### Issue tracker

Issues and specs live in GitHub Issues. Before tracker operations,
read `docs/agents/issue-tracker.md`.

### Triage labels

Before triaging issues, read the label mapping in
`docs/agents/triage-labels.md`.

### Domain docs

Single-context layout. Before exploring the codebase,
read `docs/agents/domain.md`.
