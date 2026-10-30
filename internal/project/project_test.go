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

const acceptance = `acceptance:
  dockerfile: deploy/Dockerfile
  build_args: {VITE_API_URL: "http://localhost:18080"}
  container_port: 8080
  host_port: 18080
  env: {ALLOWED_ORIGINS: "http://localhost:18081", _X1: ""}
  smoke:
    - {path: /healthz, status: 200}
    - {path: "/api/v1/items?limit=1", status: 401}
`

func TestParseAcceptance(t *testing.T) {
	c, err := project.Parse([]byte(valid), pack)
	if err != nil || c.Acceptance != nil {
		t.Fatalf("no section: %+v %v", c.Acceptance, err)
	}
	c, err = project.Parse([]byte(valid+acceptance), pack)
	if err != nil {
		t.Fatal(err)
	}
	a := c.Acceptance
	if a == nil || a.Dockerfile != "deploy/Dockerfile" || a.ContainerPort != 8080 || a.HostPort != 18080 ||
		a.BuildArgs["VITE_API_URL"] != "http://localhost:18080" || a.Env["ALLOWED_ORIGINS"] != "http://localhost:18081" ||
		!slices.Equal(a.Smoke, []project.Smoke{{Path: "/healthz", Status: 200}, {Path: "/api/v1/items?limit=1", Status: 401}}) {
		t.Fatalf("acceptance = %+v", a)
	}
	c, err = project.Parse([]byte(valid+"acceptance: {container_port: 80, host_port: 18080, smoke: [{path: /, status: 200}]}\n"), pack)
	if err != nil || c.Acceptance.Dockerfile != "Dockerfile" || c.Acceptance.Env != nil {
		t.Fatalf("defaults: %+v %v", c.Acceptance, err)
	}
}

func TestParseAcceptanceErrors(t *testing.T) {
	tests := []struct {
		name, old, new, want string
	}{
		{"unknown key", "  host_port: 18080\n", "  host_port: 18080\n  command: make deploy\n", "field command not found"},
		{"absolute dockerfile", "deploy/Dockerfile", "/etc/Dockerfile", "acceptance.dockerfile"},
		{"dotdot dockerfile", "deploy/Dockerfile", "../Dockerfile", "acceptance.dockerfile"},
		{"unclean dockerfile", "deploy/Dockerfile", "deploy/../Dockerfile", "acceptance.dockerfile"},
		{"lower-case env", "ALLOWED_ORIGINS:", "allowed_origins:", "acceptance.env: name \"allowed_origins\""},
		{"env name with a dash", "ALLOWED_ORIGINS:", "ALLOWED-ORIGINS:", "acceptance.env: name"},
		{"env name with a digit first", "ALLOWED_ORIGINS:", "1ALLOWED:", "acceptance.env: name"},
		{"multi-line env", `"http://localhost:18081"`, `"a\nb"`, "the value of ALLOWED_ORIGINS must be on one line"},
		{"build arg name", "VITE_API_URL:", "vite url:", "acceptance.build_args: name"},
		{"build arg newline", `"http://localhost:18080"`, `"x\r--privileged"`, "the value of VITE_API_URL must be on one line"},
		{"no container port", "  container_port: 8080\n", "", "container_port must be between 1 and 65535"},
		{"container port too big", "container_port: 8080", "container_port: 70000", "container_port"},
		{"privileged host port", "host_port: 18080", "host_port: 80", "host_port must be between 1024 and 65535"},
		{"host port as string", "host_port: 18080", "host_port: \"127.0.0.1:18080\"", "cannot unmarshal"},
		{"no smoke", "    - {path: /healthz, status: 200}\n    - {path: \"/api/v1/items?limit=1\", status: 401}\n", "", "acceptance.smoke needs 1 to 10 checks"},
		{"relative smoke path", "path: /healthz", "path: healthz", "acceptance.smoke[0].path"},
		{"smoke path with a space", "path: /healthz", "path: \"/health z\"", "acceptance.smoke[0].path"},
		{"smoke status", "status: 200", "status: 2000", "acceptance.smoke[0].status"},
		{"smoke unknown key", "status: 200}", "status: 200, method: POST}", "field method not found"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if !strings.Contains(acceptance, tt.old) {
				t.Fatalf("fixture does not contain %q", tt.old)
			}
			_, err := project.Parse([]byte(valid+strings.Replace(acceptance, tt.old, tt.new, 1)), pack)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("err = %v, want %q", err, tt.want)
			}
		})
	}
}

func TestParseNoPack(t *testing.T) {
	old := strings.Replace(valid, "skills_version: 0.1.0", "skills_version: 0.0.9", 1)
	if _, err := project.Parse([]byte(old), pack); err == nil {
		t.Fatal("Parse accepted another pack version")
	}
	c, err := project.ParseNoPack([]byte(old + acceptance))
	if err != nil || c.SkillsVersion != "0.0.9" || c.Acceptance == nil {
		t.Fatalf("ParseNoPack = %+v %v", c, err)
	}
	// Everything else is still checked.
	for _, bad := range []string{
		strings.Replace(old, "skills_version: 0.0.9\n", "", 1),
		strings.Replace(old+acceptance, "host_port: 18080", "host_port: 80", 1),
		old + "shell: bash\n",
	} {
		if _, err := project.ParseNoPack([]byte(bad)); err == nil {
			t.Errorf("ParseNoPack accepted:\n%s", bad)
		}
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
