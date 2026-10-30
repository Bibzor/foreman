package docker_test

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Bibzor/foreman/internal/config"
	"github.com/Bibzor/foreman/internal/project"
	"github.com/Bibzor/foreman/internal/sandbox"
	"github.com/Bibzor/foreman/internal/sandbox/docker"
)

var repo = config.Repo{Owner: "Bibzor", Name: "demo-api"}

type call struct {
	env  []string
	args []string
}

// fakeRunner records docker calls. run, when set, answers each one.
type fakeRunner struct {
	mu    sync.Mutex
	calls []call
	run   func(ctx context.Context, out io.Writer, args []string) error
}

func (f *fakeRunner) Run(ctx context.Context, env []string, out io.Writer, args ...string) error {
	f.mu.Lock()
	f.calls = append(f.calls, call{env: env, args: args})
	run := f.run
	f.mu.Unlock()
	if run != nil {
		return run(ctx, out, args)
	}
	return nil
}

type exitErr int

func (e exitErr) Error() string { return "exit" }
func (e exitErr) ExitCode() int { return int(e) }

func spec(t *testing.T) sandbox.Spec {
	t.Helper()
	p, err := project.Parse([]byte(`skills_version: 0.1.0
agent: claude
commands: {test: make test, lint: make lint}
test_paths: ["**/*_test.go"]
max_diff_lines: 50
sandbox:
  image: golang:1.27
  setup: [go mod download, go build ./...]
  cache: [/go/pkg/mod, /work/node_modules]
  env: {GOFLAGS: -mod=mod, CGO_ENABLED: "0"}
`), "0.1.0")
	if err != nil {
		t.Fatal(err)
	}
	data := t.TempDir()
	work := filepath.Join(data, "work", "Bibzor", "demo-api", "7")
	if err := os.MkdirAll(filepath.Join(work, ".git"), 0o700); err != nil {
		t.Fatal(err)
	}
	return sandbox.New(p, data, repo, 7, work, time.Minute)
}

// flag returns the values of every occurrence of flag in args.
func flag(args []string, name string) []string {
	var out []string
	for i, a := range args {
		if a == name && i+1 < len(args) {
			out = append(out, args[i+1])
		}
	}
	return out
}

// Variables a leaky implementation would copy from the host.
func poisonHost(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "ghs_leak")
	t.Setenv("FOREMAN_PRIVATE_KEY", "/keys/app.pem")
	t.Setenv("DOCKER_HOST", "unix:///var/run/docker.sock")
}

func hardened(t *testing.T, args []string, s sandbox.Spec) {
	t.Helper()
	for _, want := range [][2]string{
		{"--cap-drop", "ALL"},
		{"--security-opt", "no-new-privileges"},
		{"--user", s.User},
		{"--memory", "4g"},
		{"--memory-swap", "4g"},
		{"--cpus", "2"},
		{"--pids-limit", "1024"},
		{"--tmpfs", "/tmp:rw,exec,nosuid,nodev,size=2g"},
		{"--workdir", "/work"},
		{"--entrypoint", "go"},
	} {
		if !slices.Contains(flag(args, want[0]), want[1]) {
			t.Errorf("no %s %s in %q", want[0], want[1], args)
		}
	}
	if !slices.Contains(args, "--read-only") || !slices.Contains(args, "--rm") {
		t.Errorf("not --read-only --rm: %q", args)
	}
	if strings.HasPrefix(s.User, "0:") {
		t.Errorf("runs as root: %s", s.User)
	}
	for _, f := range []string{"--privileged", "--cap-add", "-v", "--volume", "--volumes-from", "--env-file", "--pid", "--ipc", "--device"} {
		if slices.Contains(args, f) {
			t.Errorf("%s in %q", f, args)
		}
	}
	// No environment from the host: only HOME and the sandbox env, always
	// as K=V.
	env := flag(args, "--env")
	if !slices.Equal(env, []string{"HOME=/tmp", "CGO_ENABLED=0", "GOFLAGS=-mod=mod"}) {
		t.Errorf("env = %q", env)
	}
	if strings.Contains(strings.Join(args, " "), "ghs_leak") || strings.Contains(strings.Join(args, " "), "app.pem") {
		t.Errorf("host env leaked: %q", args)
	}
}

func TestExecArgv(t *testing.T) {
	poisonHost(t)
	s := spec(t)
	f := &fakeRunner{}
	sb := &docker.Sandbox{Runner: f}
	s.Commands = []sandbox.Command{{Name: "test", Argv: []string{"go", "test", "./..."}}}
	if err := sb.Exec(context.Background(), s, "test", io.Discard); err != nil {
		t.Fatal(err)
	}
	if len(f.calls) != 1 {
		t.Fatalf("calls = %+v", f.calls)
	}
	args := f.calls[0].args
	hardened(t, args, s)
	if got := flag(args, "--network"); !slices.Equal(got, []string{"none"}) {
		t.Errorf("network = %q, want none", got)
	}
	mounts := flag(args, "--mount")
	want := []string{
		"type=bind,src=" + s.Workspace + ",dst=/work",
		"type=bind,src=" + s.Workspace + "/.git,dst=/work/.git,readonly",
		"type=bind,src=" + s.Caches[0].Host + ",dst=/go/pkg/mod,readonly",
		"type=bind,src=" + s.Caches[1].Host + ",dst=/work/node_modules,readonly",
	}
	if !slices.Equal(mounts, want) {
		t.Errorf("mounts = %q\nwant %q", mounts, want)
	}
	if tail := args[len(args)-3:]; !slices.Equal(tail, []string{"golang:1.27", "test", "./..."}) {
		t.Errorf("command = %q", tail)
	}
	if !slices.Equal(flag(args, "--label"), []string{"foreman.sandbox=exec", "foreman.repo=Bibzor/demo-api", "foreman.issue=7"}) {
		t.Errorf("labels = %q", flag(args, "--label"))
	}
	// The docker client gets the docker settings only.
	if !slices.Equal(f.calls[0].env, []string{"DOCKER_HOST=unix:///var/run/docker.sock"}) {
		t.Errorf("client env = %q", f.calls[0].env)
	}
}

func TestSetupArgv(t *testing.T) {
	poisonHost(t)
	s := spec(t)
	f := &fakeRunner{}
	sb := &docker.Sandbox{Runner: f}
	agentWork := s.Workspace
	tree := filepath.Join(filepath.Dir(filepath.Dir(s.Workspace)), "setup")
	s.Workspace = tree
	s.Commands = []sandbox.Command{{Name: "setup-0", Argv: []string{"go", "mod", "download"}}, {Name: "setup-1", Argv: []string{"go", "build", "./..."}}}
	if err := sb.Setup(context.Background(), s, io.Discard); err != nil {
		t.Fatal(err)
	}
	if len(f.calls) != 2 {
		t.Fatalf("calls = %+v", f.calls)
	}
	for _, c := range f.calls {
		args := c.args
		hardened(t, args, s)
		if got := flag(args, "--network"); !slices.Equal(got, []string{"bridge"}) {
			t.Errorf("network = %q, want bridge", got)
		}
		mounts := flag(args, "--mount")
		want := []string{
			"type=bind,src=" + tree + ",dst=/work",
			"type=bind,src=" + s.Caches[0].Host + ",dst=/go/pkg/mod",
			"type=bind,src=" + s.Caches[1].Host + ",dst=/work/node_modules",
		}
		if !slices.Equal(mounts, want) {
			t.Errorf("mounts = %q\nwant %q", mounts, want)
		}
		if strings.Contains(strings.Join(args, " "), agentWork) {
			t.Errorf("setup mounts the agent's workspace: %q", args)
		}
		if !slices.Contains(flag(args, "--label"), "foreman.sandbox=setup") {
			t.Errorf("labels = %q", flag(args, "--label"))
		}
	}
	if a := f.calls[1].args; !slices.Equal(a[len(a)-3:], []string{"golang:1.27", "build", "./..."}) {
		t.Errorf("second command = %q", a)
	}
}

func TestExecRefusesAGitSymlink(t *testing.T) {
	s := spec(t)
	if err := os.Remove(filepath.Join(s.Workspace, ".git")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(t.TempDir(), filepath.Join(s.Workspace, ".git")); err != nil {
		t.Fatal(err)
	}
	f := &fakeRunner{}
	err := (&docker.Sandbox{Runner: f}).Exec(context.Background(), s, "test", io.Discard)
	if err == nil || !strings.Contains(err.Error(), ".git is not a directory") || len(f.calls) != 0 {
		t.Fatalf("err = %v, calls = %d", err, len(f.calls))
	}
}

func TestExecUnknownName(t *testing.T) {
	f := &fakeRunner{}
	err := (&docker.Sandbox{Runner: f}).Exec(context.Background(), spec(t), "make", io.Discard)
	if err == nil || len(f.calls) != 0 {
		t.Fatalf("err = %v, calls = %d", err, len(f.calls))
	}
}

func TestExecExitCodes(t *testing.T) {
	s := spec(t)
	for _, tt := range []struct {
		err      error
		wantExit int // 0: not an ExitError
	}{
		{exitErr(2), 2},
		{exitErr(127), 127},
		{exitErr(125), 0},
		{errors.New("docker: not found"), 0},
	} {
		f := &fakeRunner{run: func(context.Context, io.Writer, []string) error { return tt.err }}
		err := (&docker.Sandbox{Runner: f}).Exec(context.Background(), s, "test", io.Discard)
		var ee *sandbox.ExitError
		got := 0
		if errors.As(err, &ee) {
			got = ee.Code
		}
		if err == nil || got != tt.wantExit {
			t.Errorf("%v: err = %v, exit = %d, want %d", tt.err, err, got, tt.wantExit)
		}
	}
}

func TestTimeoutRemovesTheContainer(t *testing.T) {
	s := spec(t)
	s.TimeoutS = 1
	f := &fakeRunner{run: func(ctx context.Context, _ io.Writer, args []string) error {
		if args[0] == "run" {
			<-ctx.Done()
			return ctx.Err()
		}
		return nil
	}}
	err := (&docker.Sandbox{Runner: f}).Exec(context.Background(), s, "test", io.Discard)
	if !errors.Is(err, sandbox.ErrTimeout) {
		t.Fatalf("err = %v", err)
	}
	if len(f.calls) != 2 || !slices.Equal(f.calls[1].args, []string{"rm", "-f", f.calls[0].args[3]}) {
		t.Fatalf("calls = %+v", f.calls)
	}
}

func TestCleanup(t *testing.T) {
	f := &fakeRunner{run: func(_ context.Context, out io.Writer, args []string) error {
		if args[0] == "ps" {
			_, _ = io.WriteString(out, "abc\ndef\n")
		}
		return nil
	}}
	if err := (&docker.Sandbox{Runner: f}).Cleanup(context.Background(), repo, 7); err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, c := range f.calls {
		got = append(got, strings.Join(c.args, " "))
	}
	want := []string{
		"ps -aq --filter label=foreman.sandbox --filter label=foreman.repo=Bibzor/demo-api --filter label=foreman.issue=7",
		"rm -f abc", "rm -f def",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("calls = %q", got)
	}
}
