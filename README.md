# foreman

A single Go binary that orchestrates AI coding agents over GitHub issues.
Today it takes an issue from opt-in to a go / no-go decision; development
by the agent comes next. It also ships the skills pack those agents use.

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

## foreman run

`foreman run` is a long-running process that polls GitHub issues and
drives them through a state machine. `foreman run --once` runs a single
polling cycle.

### Configuration

`~/.config/foreman/config.yml` (or `--config <path>`):

```yaml
app_id: 123456
private_key_path: ~/.config/foreman/app.pem
repos: ["Bibzor/demo-api", "Bibzor/demo-web"]
authorized_users: ["Bibzor"]
poll_interval: 30s                 # default
data_dir: ~/.local/share/foreman   # default
```

foreman authenticates as a GitHub App: a JWT signed with the private key,
then an installation token per repository. The installation is discovered
from the API and tokens are refreshed before they expire. The key and the
tokens are never logged or printed.

### Labels

Created on start when missing: `foreman` (opt-in), `foreman:qualifying`,
`foreman:needs-info`, `foreman:awaiting-go`, `foreman:queued`,
`foreman:blocked`, `foreman:suspect`, `go`, `no-go`.

A label counts only if the latest `labeled` event for it is by an
authorized user. Otherwise foreman ignores it and says so in one comment.
`go` and `no-go` must also be added after the issue entered its current
state. The `foreman:*` label mirrors the state stored in the database
(`data_dir/foreman.db`), which is the source of truth.

### States

```
           foreman label (authorized)
                    |
                    v
   +--------> qualifying ----------------------> blocked
   |           |     |     \   (agent failure, invalid output twice,
   |           |     |      \   3 rounds of questions)
   |           |     |       v
   |           |     |      suspect --go--> (stored result) --+
   |           |     v                                        |
   |  answer   |   needs-info                                 |
   +-----------+---- (author or authorized user comments)     |
   |           v                                              |
   |       awaiting-go <--------------------------------------+
   |        |       |
   | changed|  go   | no-go
   +--------+       +-------> closed (issue closed)
                |
                v
              queued
```

- **qualifying**: fresh shallow clone, skills installed and verified,
  Claude Code runs the `foreman-qualify` skill on the issue.
  `ready` and `out_of_scope` go to awaiting-go; `out_of_scope` comes with
  "Recommendation: no-go". The agent never closes an issue: only a
  `no-go` or a human does.
- **needs-info**: a comment by the issue author or an authorized user
  sends the issue back to qualifying. A fourth `needs_info` blocks it.
- **suspect**: the agent or the deterministic checks found something.
  `go` from an authorized user applies the stored qualification.
- **awaiting-go**: the comment shows the summary, the acceptance criteria
  as a checklist, the size, the risks and the original issue text with
  hidden content made visible.
- **Approval binds to content.** At qualification, foreman stores a
  snapshot of the title, body and comments (bot comments excluded) and
  its sha256. On `go`, it recomputes the hash from GitHub: if anything
  changed, the `go` is not applied and the issue is qualified again. The
  next steps use the stored snapshot, never the live issue.

Every transition is stored with a timestamp and a reason and posts one
comment. GitHub side effects go through an outbox in the same database
transaction, so a restart replays what was pending without duplicating
comments.

### Security notes

- The issue text is untrusted. Before it reaches the agent, invisible
  characters are shown as `<U+XXXX>` and any `untrusted_issue` delimiter
  is neutralized, so the text cannot close its
  `<untrusted_issue>` block. Checks in Go (HTML comments, zero-width,
  bidi and tag characters, long base64-looking blobs, the delimiter name)
  force `suspicious` whatever the agent says.
- The installation token is given to the `git clone` process only, as an
  HTTP header in its environment. It is never on the command line, in the
  remote URL or in `.git/config`, and `.git` is scanned for it after the
  clone. The agent's environment is rebuilt from an allowlist, so no
  GitHub credential reaches it.
- Claude Code runs with `--restricted`, the tools `Read, Grep, Glob,
  Skill`, a turn limit and a timeout. `--restricted` confines the file
  tools to the clone (symlinks pointing outside included) and ignores the
  repository's own `.claude` settings, hooks and MCP servers. The skills
  are loaded with `--plugin-dir` from a verified directory outside the
  clone.
- **This is not isolation.** The agent still runs as the local user, with
  that user's permissions and network access. The barriers above are the
  only ones today. Full isolation (a container with no network) comes in
  a later step.

## Usage

```sh
foreman run [--config path] [--once]
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
