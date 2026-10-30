package cli_test

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Bibzor/foreman/internal/cli"
	"github.com/Bibzor/foreman/internal/config"
	"github.com/Bibzor/foreman/internal/project"
	"github.com/Bibzor/foreman/internal/sandbox"
)

type exitErr int

func (e exitErr) Error() string { return "exit" }
func (e exitErr) ExitCode() int { return int(e) }

// fakeDocker records the docker command lines and answers with err,
// after writing "ran".
type fakeDocker struct {
	calls [][]string
	err   error
}

func (f *fakeDocker) Run(_ context.Context, _ []string, out io.Writer, args ...string) error {
	f.calls = append(f.calls, args)
	_, _ = io.WriteString(out, "ran\n")
	return f.err
}

// writeSpec lays out a data directory with an issue's clone and the spec
// foreman writes for it, and returns both paths.
func writeSpec(t *testing.T) (dataDir, spec string) {
	t.Helper()
	p, err := project.Parse([]byte(`skills_version: 0.1.0
agent: claude
commands: {test: make test, lint: make lint}
test_paths: ["**/*_test.go"]
max_diff_lines: 50
sandbox: {image: "golang:1.27"}
`), "0.1.0")
	if err != nil {
		t.Fatal(err)
	}
	dataDir = t.TempDir()
	work := filepath.Join(dataDir, "work", "o", "r", "1")
	skills := filepath.Join(dataDir, "skills", "o", "r", "1")
	for _, d := range []string{filepath.Join(work, ".git"), skills} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	spec, err = sandbox.Write(skills, sandbox.New(p, dataDir, config.Repo{Owner: "o", Name: "r"}, 1, work, time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	return dataDir, spec
}

func TestSandboxExec(t *testing.T) {
	dataDir, spec := writeSpec(t)
	f := &fakeDocker{}
	t.Cleanup(cli.SetSandboxRunner(f))
	t.Setenv(sandbox.EnvSpec, spec)

	code, stdout, stderr := run("sandbox", "exec", "--data-dir", dataDir, "lint")
	if code != 0 || stdout != "ran\n" {
		t.Fatalf("code %d, stdout %q, stderr %q", code, stdout, stderr)
	}
	if len(f.calls) != 1 || !slices.Contains(f.calls[0], "none") || f.calls[0][len(f.calls[0])-1] != "lint" {
		t.Fatalf("docker = %q", f.calls)
	}

	// The command's own status is foreman's.
	f.err = exitErr(3)
	if code, _, stderr := run("sandbox", "exec", "--data-dir", dataDir, "test"); code != 3 || stderr != "" {
		t.Fatalf("code %d, stderr %q", code, stderr)
	}
}

func TestSandboxExecRefuses(t *testing.T) {
	dataDir, spec := writeSpec(t)
	evil := filepath.Join(t.TempDir(), "evil.json")
	b, err := os.ReadFile(spec)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(evil, b, 0o600); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name     string
		spec     string
		args     []string
		wantCode int
		want     string
	}{
		{"no spec", "", []string{"--data-dir", dataDir, "test"}, 1, "FOREMAN_SANDBOX_SPEC is not set"},
		{"spec outside skills", evil, []string{"--data-dir", dataDir, "test"}, 1, "is not under"},
		{"spec under another data dir", spec, []string{"--data-dir", t.TempDir(), "test"}, 1, "is not under"},
		{"unknown name", spec, []string{"--data-dir", dataDir, "make"}, 2, `no sandbox command "make"`},
		{"raw command", spec, []string{"--data-dir", dataDir, "make", "test"}, 2, "exactly one command name"},
		{"no data dir", spec, []string{"test"}, 2, "needs --data-dir"},
		{"other subcommand", spec, nil, 2, "unknown sandbox command"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := &fakeDocker{}
			t.Cleanup(cli.SetSandboxRunner(f))
			t.Setenv(sandbox.EnvSpec, tt.spec)
			args := append([]string{"sandbox", "exec"}, tt.args...)
			if tt.args == nil {
				args = []string{"sandbox", "run"}
			}
			code, _, stderr := run(args...)
			if code != tt.wantCode || !strings.Contains(stderr, tt.want) {
				t.Fatalf("code %d, stderr %q", code, stderr)
			}
			if len(f.calls) != 0 {
				t.Fatalf("docker ran: %q", f.calls)
			}
		})
	}
}
