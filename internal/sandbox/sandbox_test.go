package sandbox_test

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/Bibzor/foreman/internal/config"
	"github.com/Bibzor/foreman/internal/project"
	"github.com/Bibzor/foreman/internal/sandbox"
)

var repo = config.Repo{Owner: "Bibzor", Name: "demo-api"}

const projectFile = `skills_version: 0.1.0
agent: claude
commands: {test: make test, lint: make lint, extra: [make vuln]}
test_paths: ["**/*_test.go"]
max_diff_lines: 50
sandbox:
  image: golang:1.27
  setup: [go mod download]
  cache: [/go/pkg/mod]
  env: {GOFLAGS: -mod=mod}
`

// layout is a data directory with an issue's workspace (with its .git),
// cache and plugin directory, and the spec written there.
type layout struct {
	data, work, skills, spec string
	s                        sandbox.Spec
}

func newLayout(t *testing.T) layout {
	t.Helper()
	p, err := project.Parse([]byte(projectFile), "0.1.0")
	if err != nil {
		t.Fatal(err)
	}
	data := t.TempDir()
	l := layout{
		data:   data,
		work:   filepath.Join(data, "work", "Bibzor", "demo-api", "7"),
		skills: filepath.Join(data, "skills", "Bibzor", "demo-api", "7"),
	}
	for _, d := range []string{filepath.Join(l.work, ".git"), l.skills} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	l.s = sandbox.New(p, data, repo, 7, l.work, sandbox.AgentTimeout)
	if err := sandbox.ResetCaches(data, repo, 7, l.s.Caches); err != nil {
		t.Fatal(err)
	}
	if l.spec, err = sandbox.Write(l.skills, l.s); err != nil {
		t.Fatal(err)
	}
	return l
}

func TestSpecRoundTrip(t *testing.T) {
	l := newLayout(t)
	if l.spec != filepath.Join(l.skills, "sandbox.json") || strings.HasPrefix(l.spec, l.work) {
		t.Fatalf("spec at %s, want the plugin directory, outside the clone %s", l.spec, l.work)
	}
	fi, err := os.Stat(l.spec)
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("spec mode: %v %v", fi, err)
	}
	s, err := sandbox.Read(l.spec, l.data)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, c := range s.Commands {
		names = append(names, c.Name+"="+strings.Join(c.Argv, " "))
	}
	if !slices.Equal(names, []string{"test=make test", "lint=make lint", "0=make vuln"}) {
		t.Errorf("commands = %q", names)
	}
	if s.Image != "golang:1.27" || s.Workspace != l.work || s.Env["GOFLAGS"] != "-mod=mod" || s.Timeout() != sandbox.AgentTimeout ||
		len(s.Caches) != 1 || s.Caches[0].Container != "/go/pkg/mod" ||
		s.Caches[0].Host != filepath.Join(l.data, "cache", "Bibzor", "demo-api", "7", "0") {
		t.Errorf("spec = %+v", s)
	}
	if strings.HasPrefix(s.User, "0:") {
		t.Errorf("user = %s", s.User)
	}
}

func TestReadRefuses(t *testing.T) {
	tests := []struct {
		name  string
		setup func(t *testing.T, l layout) (path, dataDir string)
		want  string
	}{
		{"unset", func(*testing.T, layout) (string, string) { return "", "/x" }, "FOREMAN_SANDBOX_SPEC is not set"},
		{"relative", func(_ *testing.T, l layout) (string, string) { return "sandbox.json", l.data }, "must be absolute"},
		{"outside skills", func(t *testing.T, l layout) (string, string) {
			p := filepath.Join(l.data, "evil.json")
			copyFile(t, l.spec, p)
			return p, l.data
		}, "is not under"},
		{"in /tmp", func(t *testing.T, l layout) (string, string) {
			p := filepath.Join(t.TempDir(), "evil.json")
			copyFile(t, l.spec, p)
			return p, l.data
		}, "is not under"},
		{"in the workspace", func(t *testing.T, l layout) (string, string) {
			p := filepath.Join(l.work, "sandbox.json")
			copyFile(t, l.spec, p)
			return p, l.data
		}, "is not under"},
		{"symlink from skills to outside", func(t *testing.T, l layout) (string, string) {
			out := filepath.Join(t.TempDir(), "evil.json")
			copyFile(t, l.spec, out)
			p := filepath.Join(l.skills, "link.json")
			if err := os.Symlink(out, p); err != nil {
				t.Fatal(err)
			}
			return p, l.data
		}, "is not under"},
		{"another data dir", func(t *testing.T, l layout) (string, string) { return l.spec, t.TempDir() }, "is not under"},
		{"mode 0644", func(t *testing.T, l layout) (string, string) {
			if err := os.Chmod(l.spec, 0o644); err != nil {
				t.Fatal(err)
			}
			return l.spec, l.data
		}, "want 0600"},
		{"directory", func(t *testing.T, l layout) (string, string) {
			p := filepath.Join(l.skills, "dir.json")
			if err := os.Mkdir(p, 0o700); err != nil {
				t.Fatal(err)
			}
			return p, l.data
		}, "not a regular file"},
		{"another owner", func(t *testing.T, l layout) (string, string) {
			t.Cleanup(sandbox.SetUID(os.Getuid() + 1))
			return l.spec, l.data
		}, "not owned by the current user"},
		{"unknown field", func(t *testing.T, l layout) (string, string) {
			rewrite(t, l.spec, `"version": 1,`, `"version": 1, "privileged": true,`)
			return l.spec, l.data
		}, "unknown field"},
		{"shell in argv", func(t *testing.T, l layout) (string, string) {
			rewrite(t, l.spec, `"make",`, `"sh", "-c", "make;curl",`)
			return l.spec, l.data
		}, "only letters, digits"},
		{"latest image", func(t *testing.T, l layout) (string, string) {
			rewrite(t, l.spec, `golang:1.27`, `golang:latest`)
			return l.spec, l.data
		}, "not latest"},
		{"workspace outside the data dir", func(t *testing.T, l layout) (string, string) {
			other := t.TempDir()
			rewrite(t, l.spec, `"workspace": "`+l.work, `"workspace": "`+other)
			return l.spec, l.data
		}, "must be under"},
		{"cache outside the data dir", func(t *testing.T, l layout) (string, string) {
			rewrite(t, l.spec, `"host": "`+l.s.Caches[0].Host, `"host": "`+t.TempDir())
			return l.spec, l.data
		}, "must be under"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			l := newLayout(t)
			path, dataDir := tt.setup(t, l)
			_, err := sandbox.Read(path, dataDir)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("err = %v, want %q", err, tt.want)
			}
		})
	}
}

func TestValidateRefusesRootAndBadDockerEnv(t *testing.T) {
	l := newLayout(t)
	s := l.s
	s.User = "0:0"
	if err := s.Validate(); err == nil || !strings.Contains(err.Error(), "not root") {
		t.Errorf("root: %v", err)
	}
	s = l.s
	s.DockerEnv = []string{"LD_PRELOAD=/x"}
	if err := s.Validate(); err == nil || !strings.Contains(err.Error(), "docker env") {
		t.Errorf("docker env: %v", err)
	}
	s = l.s
	s.Commands = append(s.Commands, sandbox.Command{Name: "test", Argv: []string{"true"}})
	if err := s.Validate(); err == nil || !strings.Contains(err.Error(), `command name "test"`) {
		t.Errorf("duplicate: %v", err)
	}
}

func TestLine(t *testing.T) {
	if got := sandbox.Line("/usr/local/bin/foreman", "/srv/foreman", "test"); got != "/usr/local/bin/foreman sandbox exec --data-dir /srv/foreman test" {
		t.Fatal(got)
	}
	for _, bad := range []string{"relative", "/a b", "/a/../b", "/a:b", "/a,b", "/a/", "/a*"} {
		if sandbox.HostPath(bad) == nil {
			t.Errorf("HostPath(%q) accepted", bad)
		}
	}
}

func TestResetCachesRemovesReadOnlyTrees(t *testing.T) {
	l := newLayout(t)
	ro := filepath.Join(l.s.Caches[0].Host, "mod", "x@v1")
	if err := os.MkdirAll(ro, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ro, "go.mod"), []byte("module x\n"), 0o400); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(ro, 0o500); err != nil { // like the Go module cache
		t.Fatal(err)
	}
	if err := sandbox.ResetCaches(l.data, repo, 7, l.s.Caches); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(l.s.Caches[0].Host)
	if err != nil || len(entries) != 0 {
		t.Fatalf("cache not emptied: %v %v", entries, err)
	}
}

func copyFile(t *testing.T, from, to string) {
	t.Helper()
	b, err := os.ReadFile(from)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(to, b, 0o600); err != nil {
		t.Fatal(err)
	}
}

func rewrite(t *testing.T, path, from, to string) {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), from) {
		t.Fatalf("%s does not contain %q:\n%s", path, from, b)
	}
	if err := os.WriteFile(path, []byte(strings.Replace(string(b), from, to, 1)), 0o600); err != nil {
		t.Fatal(err)
	}
}
