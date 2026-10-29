package project_test

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/Bibzor/foreman/internal/project"
)

const pack = "0.1.0"

const valid = `skills_version: 0.1.0
agent: claude
commands:
  test: make test
  lint: make  lint
  extra: [make vuln, go tool govulncheck ./...]
protected_paths: [.github/workflows/, AGENTS.md, .foreman.yml, go.mod, go.sum]
test_paths: ["**/*_test.go", "src/**/*.test.tsx", "testdata/**"]
max_diff_lines: 400
`

func TestParseValid(t *testing.T) {
	c, err := project.Parse([]byte(valid), pack)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(c.Test.Argv, []string{"make", "test"}) || c.Lint.Raw != "make lint" || len(c.Extra) != 2 {
		t.Errorf("commands = %+v %+v %+v", c.Test, c.Lint, c.Extra)
	}
	if got := c.Extra[1].Argv; !slices.Equal(got, []string{"go", "tool", "govulncheck", "./..."}) {
		t.Errorf("extra argv = %q", got)
	}
	if c.MaxDiffLines != 400 || len(c.Commands()) != 4 {
		t.Errorf("config = %+v", c)
	}
	for p, want := range map[string]bool{
		".github/workflows/ci.yml": true,
		".github/dependabot.yml":   false,
		"AGENTS.md":                true,
		"docs/AGENTS.md":           false,
		"go.sum":                   true,
	} {
		if got := c.IsProtected(p); got != want {
			t.Errorf("IsProtected(%q) = %v", p, got)
		}
	}
}

func TestParseErrors(t *testing.T) {
	tests := []struct {
		name, old, new, want string
	}{
		{"pack mismatch", "skills_version: 0.1.0", "skills_version: 0.2.0", "project pins 0.2.0, foreman ships 0.1.0"},
		{"no skills_version", "skills_version: 0.1.0\n", "", "skills_version is required"},
		{"unknown agent", "agent: claude", "agent: gpt", "unknown agent"},
		{"unsupported agent", "agent: claude", "agent: codex", "not supported yet"},
		{"no test", "  test: make test\n", "", "commands.test is required"},
		{"unknown key", "max_diff_lines: 400", "max_diff_lines: 400\nshell: bash", "field shell not found"},
		{"pipe", "make test", "make test | tee out", "no shell syntax"},
		{"and", "make test", "make test && curl x", "no shell syntax"},
		{"semicolon", "make test", "make test;rm", "no shell syntax"},
		{"variable", "make test", "make $TARGET", "no shell syntax"},
		{"subshell", "make test", "make $(id)", "no shell syntax"},
		{"backtick", "make test", "make `id`", "no shell syntax"},
		{"redirect", "make test", "make test >out", "no shell syntax"},
		{"quote", "make test", "make 'test'", "no shell syntax"},
		{"glob", "make test", "go test ./*", "no shell syntax"},
		{"tab", "make test", "make\ttest", "no shell syntax"},
		{"parenthesis in extra", "make vuln", "make vuln(x)", "commands.extra[0]"},
		{"option first", "make test", "-v test", "must start with a program name"},
		{"absolute protected", "AGENTS.md,", "/etc/passwd,", "protected_paths"},
		{"dotdot protected", "AGENTS.md,", "../x,", "protected_paths"},
		{"glob protected", "AGENTS.md,", "\"docs/*.md\",", "protected_paths"},
		{"no test_paths", `test_paths: ["**/*_test.go", "src/**/*.test.tsx", "testdata/**"]`, "test_paths: []", "test_paths is required"},
		{"absolute glob", `"testdata/**"`, `"/abs/*.go"`, "must be relative"},
		{"dotdot glob", `"testdata/**"`, `"../x/*_test.go"`, `no "." or ".." segment`},
		{"partial double star", `"testdata/**"`, `"a**b"`, `"**" must be a whole path segment`},
		{"bad class", `"testdata/**"`, `"x/[.go"`, "syntax error in pattern"},
		{"empty segment", `"testdata/**"`, `"a//b"`, "empty path segment"},
		{"backslash", `"testdata/**"`, `"a\\b"`, "no backslash"},
		{"empty glob", `"testdata/**"`, `""`, "empty pattern"},
		{"zero diff", "max_diff_lines: 400", "max_diff_lines: 0", "max_diff_lines"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if !strings.Contains(valid, tt.old) {
				t.Fatalf("fixture does not contain %q", tt.old)
			}
			_, err := project.Parse([]byte(strings.Replace(valid, tt.old, tt.new, 1)), pack)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("err = %v, want %q", err, tt.want)
			}
		})
	}
}

func TestParseMissing(t *testing.T) {
	if _, err := project.Parse(nil, pack); !errors.Is(err, project.ErrMissing) {
		t.Fatalf("err = %v", err)
	}
}

func TestMatch(t *testing.T) {
	tests := []struct {
		glob, path string
		want       bool
	}{
		{"**/*_test.go", "x_test.go", true},
		{"**/*_test.go", "a/b/x_test.go", true},
		{"**/*_test.go", "a/x.go", false},
		{"**/*_test.go", "a/x_test.go/y", false},
		{"src/**/*.test.tsx", "src/a.test.tsx", true},
		{"src/**/*.test.tsx", "src/ui/deep/a.test.tsx", true},
		{"src/**/*.test.tsx", "lib/a.test.tsx", false},
		{"src/**/*.test.tsx", "src/a.tsx", false},
		{"testdata/**", "testdata/x/y.json", true},
		{"testdata/**", "testdata", true},
		{"testdata/**", "pkg/testdata/x", false},
		{"*_test.go", "a/x_test.go", false},
		{"tests/*.py", "tests/test_a.py", true},
		{"tests/*.py", "tests/sub/test_a.py", false},
	}
	for _, tt := range tests {
		if got := project.Match(tt.glob, tt.path); got != tt.want {
			t.Errorf("Match(%q, %q) = %v, want %v", tt.glob, tt.path, got, tt.want)
		}
	}
	c, err := project.Parse([]byte(valid), pack)
	if err != nil {
		t.Fatal(err)
	}
	if !c.IsTestPath("internal/x/y_test.go") || c.IsTestPath("internal/x/y.go") {
		t.Error("IsTestPath")
	}
}
