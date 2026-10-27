---
name: go-conventions
description: Go coding rules and the standard commands to format, lint, test and scan a Go repository. Use when writing or reviewing Go code.
---

# Go conventions

Follow AGENTS.md and the style already in the repository first. These
rules apply where they are silent.

## Code

- Format with gofmt and goimports. Never hand-format.
- Small packages with one clear purpose. Code that is not an API goes in
  `internal/`.
- Return errors, do not panic. Wrap with context:
  `fmt.Errorf("read config: %w", err)`. Compare with `errors.Is` and
  `errors.As`, never by string.
- Handle every error, or discard it explicitly with `_ =` and a comment.
- Pass `context.Context` as the first argument of anything that does I/O
  or can block. Do not store it in structs.
- Accept interfaces, return concrete types. Define interfaces where they
  are used, not where they are implemented.
- No mutable package-level state except values set once at build time.
- Prefer the standard library. Add a dependency only when it saves real
  work, and say why in the PR.
- Doc comments on every exported name, starting with the name.
- Names are short and clear; no `util` or `common` packages.

## Tests

- Table-driven tests with `t.Run` and a name per case.
- Test behaviour through the exported API. Use `t.TempDir()`,
  `httptest` and `testing/fstest` instead of real resources.
- Failure messages state got and want:
  `t.Errorf("Parse(%q) = %v, want %v", in, got, want)`.
- Tests must pass with `-race` and must not depend on order or timing.

## Commands

```sh
gofmt -l .                     # must print nothing
go vet ./...
golangci-lint run              # uses .golangci.yml
go test -race ./...
go tool govulncheck ./...      # when declared as a tool in go.mod
go mod tidy                    # go.mod and go.sum must not change
```

Run all of them before committing. A change is not done while any of
them fails.
