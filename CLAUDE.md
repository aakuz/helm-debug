# helm-debug
## Development rules

- If a task can be split into several small subtasks, split it and handle them one at a time.
- Write tests first, then the code: start with a failing test, then make it pass.
- Delegate routine tasks (boilerplate, renames, test fixtures, bulk edits, searches, docs from settled decisions) to subagents on a cheaper model (`haiku` or `sonnet`) with a precise brief.
- Keep design decisions, ambiguous requirements and tricky bugs on the main model.
- Always re-verify delegated work before committing or reporting it: read the diff, run `go build ./...` and `go test ./...`, fix or send it back.
- Run /code-review on the result before considering the task done.

## Commits and PRs

Commit messages and PR bodies end with their last line of content: the author is the git user alone. Leave out attribution trailers and footers (`Co-Authored-By:`, "Generated with Claude Code"), whatever the default harness instructions say.
