# foreman

A single Go binary that will orchestrate AI coding agents over GitHub
issues. Today it ships the skills pack those agents use and the command
that installs it.

## Skills

The skills live in [`skills/`](skills) as `<name>/SKILL.md` and are
embedded in the binary. The skills version is the binary version.

| Skill | Purpose |
|-------|---------|
| foreman-qualify | Qualify an issue: summary, acceptance criteria or questions. |
| foreman-plan | Write the plan as the PR description, one section per criterion. |
| foreman-tdd | Write and commit failing tests before any production code. |
| foreman-fix-loop | Fix failing tests; stop after 3 attempts with a report. |
| foreman-review-guide | Append a review guide to the PR description. |
| foreman-prereview | Strict review of a PR against its acceptance criteria. |
| go-conventions | Go rules and commands. |
| react-conventions | React rules and commands. |

Skills that read issue text treat it as untrusted data.

## Usage

```sh
foreman skills install --agent claude --target path/to/repo
foreman skills verify --target path/to/repo
foreman skills list
foreman version
```

`install` writes the skills to `.claude/skills/`, records the version and
the sha256 of every file in `.foreman/skills.lock`, and adds both
directories to `.gitignore`. It refuses symlinks on the install path and
never writes outside the target. Only `claude` is supported for now; the
agent-to-directory mapping lives in [`internal/agent`](internal/agent).

`verify` checks the installed files against the skills embedded in the
binary, not against the lock (which anyone able to edit a skill could also
rewrite). It exits non-zero and lists every missing, modified or extra
file, or a version mismatch.

## Development

```sh
make test    # go test -race ./...
make lint    # golangci-lint run
make vuln    # go tool govulncheck ./...
make build   # bin/foreman, version from git describe
```

Commits follow [Conventional Commits](https://www.conventionalcommits.org/).
