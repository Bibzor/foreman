# foreman

A single Go binary that orchestrates AI coding agents over GitHub issues.
Today it takes an issue from opt-in to a go / no-go decision, then has
the agent develop it into a pull request ready for human review, and
follows that review until the PR is merged. Acceptance after the merge
comes next. It also ships the skills pack those agents use.

## Skills

The skills live in [`skills/`](skills) as `<name>/SKILL.md` and are
embedded in the binary. The pack has its own version,
`skills.PackVersion` (semver, in [`skills/skills.go`](skills/skills.go)),
independent of the binary version, which only `foreman version` reports.
A pull request that changes a skill bumps `PackVersion` and updates
`PackSHA256` next to it: a test fails if the pack changes under an
unchanged version. Projects pin the pack with `skills_version` in their
`.foreman.yml`.

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
wip_limit: 2                       # default: issues in development or review, all repos together
```

foreman authenticates as a GitHub App: a JWT signed with the private key,
then an installation token per repository. The installation is discovered
from the API and tokens are refreshed before they expire. The key and the
tokens are never logged or printed. The App needs these repository
permissions: Issues (read and write), Contents (read and write, to push
`foreman/<issue>` branches) and Pull requests (read and write).

### Project configuration: `.foreman.yml`

Each project commits `.foreman.yml` at the root of its default branch:

```yaml
skills_version: 0.1.0          # must equal the skills pack foreman ships
agent: claude                  # the only agent supported for now
commands:
  test: make test
  lint: make lint
  extra: [make vuln]           # optional: more commands the agent may run
protected_paths: [.github/workflows/, AGENTS.md, .foreman.yml, go.mod, go.sum]
test_paths: ["**/*_test.go"]   # globs; "**" matches any number of directories
max_diff_lines: 400
```

foreman reads it from the default branch, never from a work branch, so
the agent cannot change its own rules. A missing or invalid file blocks
the issue with the reason. Commands are plain words (letters, digits and
`_./:=@%+,-`) run as argv: no shell, quoting, variables, pipes or
redirections. A path in `protected_paths` ending in `/` covers everything
under it; any other is an exact file. `skills_version` must equal the
pack foreman ships, otherwise: "project pins X, foreman ships Y".

### Labels

Created on start when missing: `foreman` (opt-in), `foreman:qualifying`,
`foreman:needs-info`, `foreman:awaiting-go`, `foreman:queued`,
`foreman:in-progress`, `foreman:in-review`, `foreman:merged`,
`foreman:blocked`, `foreman:suspect`, `go`, `no-go`, and
`foreman:high-risk`, which goes on pull requests.

A label counts only if the latest `labeled` event for it is by an
authorized user. Otherwise foreman ignores it and says so in one comment.
`go` and `no-go` must also be added after the comment that announced the
current state, both times taken from GitHub's clock. The `foreman:*`
label of an issue mirrors the state stored in the database
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
              queued  (waits while wip_limit issues are in progress or in review)
                |
                v
           in-progress ------------------------> blocked
                |        (agent failed, invalid output twice,
                |         a check failed, .foreman.yml missing or invalid)
                v
            in-review  <--- changes requested by an authorized user:
             |     |        one revision, at most 2 ----> blocked
             |     +--- PR closed without merge --------> blocked
             v
           merged  (the issue stays open until acceptance)
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
- **queued**: development starts when fewer than `wip_limit` issues, all
  repositories together, are in progress or in review. Otherwise foreman
  says why, once.
- **in-progress**: a fresh clone of the default branch, a new branch
  `foreman/<issue>`. Claude Code gets the stored snapshot inside
  `<untrusted_issue>`, the approved summary and acceptance criteria as
  trusted input, and follows `foreman-plan`, `foreman-tdd`,
  `foreman-fix-loop` and `foreman-review-guide`. It ends with a JSON
  object `{status, plan, review_guide, failure_report}`, validated like
  the qualification output. Then foreman checks the branch itself,
  without trusting the agent; any failure blocks the issue and names the
  check:
  1. `commit-order`: the branch starts with `test:` commits that touch
     only files matching `test_paths`, and no later commit touches a test
     file. The history is linear.
  2. `protected-paths`: no file under `protected_paths` changed.
  3. `diff-size`: at most `max_diff_lines` added plus deleted lines.
  4. `test` and `lint`: foreman runs `commands.test` and `commands.lint`
     itself and they pass.

  Then it pushes the branch and opens a pull request: the issue title,
  the plan and the review guide, and `Refs #<issue>`, never `Closes`, so
  the issue stays open until acceptance.
- **in-review**: right after the PR opens, a separate Claude Code run, in
  a fresh session with read-only tools, runs `foreman-prereview` on the
  diff and the acceptance criteria and posts its findings on the PR. A
  `high` finding, a pre-review that could not run, or a change to a
  dependency file or an authentication-related path adds
  `foreman:high-risk` to the PR. Then:
  - PR merged: **merged**. Nothing more happens today.
  - PR closed without merge: **blocked**.
  - a review requesting changes by an authorized user: one revision run
    on the existing branch, with the review as untrusted input, the same
    checks, a fast-forward push and a comment on the PR. A revision may
    change existing tests only in `test:` commits that touch test files
    only, and the comment lists every test file changed. A third request
    for changes blocks the issue. Reviews by anyone else are ignored.

Every transition is stored with a timestamp and a reason and posts one
comment. GitHub side effects, opening the PR included, go through an
outbox in the same database transaction, so a restart replays what was
pending without duplicating comments or pull requests. A workspace that
cannot be prepared, or a branch that cannot be pushed, is retried on the
next cycle; after 5 failures in a row the issue is blocked with the last
error.

### Security notes

- **Tests and lint now execute code written by the agent, on this host,
  as the local user, without isolation.** That code can read and change
  anything the user can: the GitHub App private key, the foreman
  database, other repositories' workspaces, SSH keys. It can also reach
  the network. The restrictions below apply to the agent's tools, not to
  the code those commands run. Run foreman as a dedicated user with
  nothing else to lose. Container isolation comes in the next step.
- The issue text, review comments, the diff and commit messages are
  untrusted. Before they reach an agent, invisible characters are shown
  as `<U+XXXX>` and the `untrusted_issue`, `untrusted_review` and
  `untrusted_diff` delimiters are neutralized, so the text cannot close
  its block. Checks in Go (HTML comments, zero-width, bidi and tag
  characters, long base64-looking blobs, the delimiter names) force
  `suspicious` whatever the agent says. Agent-written text posted on
  GitHub has HTML comments and mentions neutralized.
- The installation token is given to the `git clone`, `git fetch` and
  `git push` processes only, as an HTTP header in their environment. It
  is never on the command line, in a remote URL or in `.git/config`, and
  `.git` is scanned for it afterwards. The agent's environment and the
  test and lint commands' environment are rebuilt from allowlists, so no
  GitHub credential reaches them.
- Claude Code runs with `--restricted`, a turn limit and a timeout.
  `--restricted` confines the file tools to the clone (symlinks pointing
  outside included) and ignores the repository's own `.claude` settings,
  hooks and MCP servers. The skills are loaded with `--plugin-dir` from a
  verified directory outside the clone. Qualification and pre-review get
  `Read, Grep, Glob, Skill`. Development gets `Read, Grep, Glob, Edit,
  Write, Skill`, and `Bash` only for the project commands, verbatim, and
  `git add`, `git commit`, `git status`, `git diff`, `git log`. No push,
  no `gh`, no web tools. Its git runs with no user configuration and no
  hooks.
- foreman never runs git in the agent's clone after the agent ran. It
  clones the branch into a directory it owns, so only committed history
  crosses over: the agent's `.git` configuration, hooks and uncommitted
  files stay behind. Every check and the push run there, and foreman
  pushes the exact commit it checked.
- `.foreman.yml` is read from the default branch. Commands run as argv,
  never through a shell.
- **The agent itself is not isolated either.** It runs as the local user,
  with that user's permissions and network access. The barriers above
  are the only ones today.

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

`verify` checks the installed files and the lock version against the
skills embedded in the binary, not against the lock hashes (which anyone
able to edit a skill could also rewrite). It exits non-zero and lists every missing, modified or extra
file, or a version mismatch.

## Development

```sh
make test    # go test -race ./...
make lint    # golangci-lint run
make vuln    # go tool govulncheck ./...
make build   # bin/foreman, version from git describe
```

Commits follow [Conventional Commits](https://www.conventionalcommits.org/).
