# Issue tracker: GitHub

Issues and specs live in GitHub Issues for `aakuz/helm-debug`.
Use the `gh` CLI from this repository.

## Operations

- Read: `gh issue view <number> --json number,title,body,labels,comments`.
- List: `gh issue list --state open --json number,title,body,labels`.
  Add label and state filters as needed.
- Create: `gh issue create --title "..." --body-file <path>`.
- Comment: `gh issue comment <number> --body-file <path>`.
- Labels: `gh issue edit <number> --add-label "..."` or
  `--remove-label "..."`.
- Close: `gh issue close <number>`.

For multiline bodies, write the content to a temporary file
and pass it with `--body-file`.

When a skill says "publish to the issue tracker", create an issue.
When it says "fetch the relevant ticket", read the referenced issue.

## Pull requests as a triage surface

**PRs as a request surface: no.**

GitHub issues and pull requests share a number space.
For an ambiguous reference, resolve whether it is an issue or PR
before reading or changing it.
