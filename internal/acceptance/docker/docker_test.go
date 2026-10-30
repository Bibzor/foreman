package docker_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Bibzor/foreman/internal/acceptance"
	"github.com/Bibzor/foreman/internal/acceptance/docker"
	"github.com/Bibzor/foreman/internal/config"
	"github.com/Bibzor/foreman/internal/project"
)

// fakeRunner records every docker call; fail maps a subcommand to the
// output and error it returns.
type fakeRunner struct {
	mu    sync.Mutex
	calls [][]string
	ps    string
	fail  map[string]string
}

func (r *fakeRunner) Run(_ context.Context, args ...string) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, slices.Clone(args))
	if out, ok := r.fail[args[0]]; ok {
		return out, errors.New("exit status 1")
	}
	if args[0] == "ps" {
		return r.ps, nil
	}
	return "", nil
}

func (r *fakeRunner) subcommands() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []string
	for _, c := range r.calls {
		out = append(out, strings.Join(c[:min(len(c), 2)], " "))
	}
	return out
}

func (r *fakeRunner) call(sub string) []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, c := range r.calls {
		if c[0] == sub {
			return c
		}
	}
	return nil
}

func spec(t *testing.T) acceptance.Spec {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte("FROM scratch\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return acceptance.Spec{
		Repo: config.Repo{Owner: "Bibzor", Name: "demo-api"}, Issue: 7,
		SHA: "0123456789abcdef0123456789abcdef01234567", Dir: dir,
		Config: project.Acceptance{
			Dockerfile: "Dockerfile", ContainerPort: 8080, HostPort: 18080,
			BuildArgs: map[string]string{"VITE_API_URL": "http://localhost:18080", "A": "1"},
			Env:       map[string]string{"ALLOWED_ORIGINS": "http://localhost:18081", "B": "x y"},
			Smoke:     []project.Smoke{{Path: "/healthz", Status: 200}},
		},
	}
}

func TestNames(t *testing.T) {
	image, name := docker.Names(config.Repo{Owner: "Bibzor", Name: "Demo.API_x"}, "0123456789ABCDEF")
	if image != "foreman-acc/bibzor-demo-api-x:0123456789ab" || name != "foreman-acc-bibzor-demo-api-x" {
		t.Fatalf("Names = %q %q", image, name)
	}
}

func TestArgv(t *testing.T) {
	s := spec(t)
	build := docker.BuildArgv(s)
	want := []string{"build",
		"--label", "foreman.repo=Bibzor/demo-api", "--label", "foreman.issue=7", "--label", "foreman.sha=" + s.SHA,
		"--build-arg", "A=1", "--build-arg", "VITE_API_URL=http://localhost:18080",
		"-t", "foreman-acc/bibzor-demo-api:0123456789ab", "-f", filepath.Join(s.Dir, "Dockerfile"), s.Dir}
	if !slices.Equal(build, want) {
		t.Errorf("build argv =\n%q\nwant\n%q", build, want)
	}
	run := docker.RunArgv(s)
	want = []string{"run", "-d", "--name", "foreman-acc-bibzor-demo-api",
		"--label", "foreman.repo=Bibzor/demo-api", "--label", "foreman.issue=7", "--label", "foreman.sha=" + s.SHA,
		"-p", "127.0.0.1:18080:8080",
		"-e", "ALLOWED_ORIGINS=http://localhost:18081", "-e", "B=x y",
		"--read-only", "--tmpfs", "/tmp", "--cap-drop", "ALL", "--security-opt", "no-new-privileges",
		"--memory", "512m", "--pids-limit", "256",
		"foreman-acc/bibzor-demo-api:0123456789ab"}
	if !slices.Equal(run, want) {
		t.Errorf("run argv =\n%q\nwant\n%q", run, want)
	}
	for _, a := range append(build, run...) {
		if a == "sh" || a == "-c" || a == "--privileged" || strings.HasPrefix(a, "0.0.0.0") {
			t.Errorf("argv has %q", a)
		}
	}
}

func TestDeployReplacesThePreviousContainer(t *testing.T) {
	r := &fakeRunner{ps: "abc123\ndef456\n"}
	d := &docker.Deployer{Runner: r}
	dep, err := d.Deploy(context.Background(), spec(t))
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"version --format", "build --label", "ps -aq", "rm -f", "rm -f", "run -d"}
	if got := r.subcommands(); !slices.Equal(got, want) {
		t.Fatalf("calls = %q, want %q", got, want)
	}
	if ps := r.call("ps"); !slices.Equal(ps, []string{"ps", "-aq", "--filter", "label=foreman.repo=Bibzor/demo-api"}) {
		t.Errorf("ps = %q", ps)
	}
	if r.calls[3][2] != "abc123" || r.calls[4][2] != "def456" {
		t.Errorf("removed %q and %q", r.calls[3], r.calls[4])
	}
	if dep.URL != "http://localhost:18080" || dep.ID != "foreman-acc-bibzor-demo-api" {
		t.Errorf("deployment = %+v", dep)
	}
}

func TestDeployFailures(t *testing.T) {
	t.Run("docker unavailable", func(t *testing.T) {
		r := &fakeRunner{fail: map[string]string{"version": "Cannot connect to the Docker daemon"}}
		_, err := (&docker.Deployer{Runner: r}).Deploy(context.Background(), spec(t))
		if !errors.Is(err, acceptance.ErrUnavailable) || len(r.calls) != 1 {
			t.Fatalf("err = %v, calls = %q", err, r.subcommands())
		}
	})
	t.Run("build", func(t *testing.T) {
		r := &fakeRunner{fail: map[string]string{"build": "step 2: RUN make\ntoken ghs_AbCdEfGhIjKlMnOpQrStUvWx0123\nfailed"}}
		_, err := (&docker.Deployer{Runner: r}).Deploy(context.Background(), spec(t))
		var se *acceptance.StageError
		if !errors.As(err, &se) || se.Stage != "build" || !strings.Contains(se.Log, "step 2: RUN make") || strings.Contains(se.Log, "ghs_") {
			t.Fatalf("err = %#v", err)
		}
		if slices.Contains(r.subcommands(), "run -d") {
			t.Error("ran after a failed build")
		}
	})
	t.Run("run", func(t *testing.T) {
		r := &fakeRunner{fail: map[string]string{"run": "port is already allocated"}}
		_, err := (&docker.Deployer{Runner: r}).Deploy(context.Background(), spec(t))
		var se *acceptance.StageError
		if !errors.As(err, &se) || se.Stage != "run" || !strings.Contains(se.Log, "port is already allocated") {
			t.Fatalf("err = %#v", err)
		}
		if last := r.calls[len(r.calls)-1]; !slices.Equal(last, []string{"rm", "-f", "foreman-acc-bibzor-demo-api"}) {
			t.Errorf("last call = %q: a failed run is cleaned up", last)
		}
	})
	t.Run("dockerfile outside the repository", func(t *testing.T) {
		s := spec(t)
		if err := os.Remove(filepath.Join(s.Dir, "Dockerfile")); err != nil {
			t.Fatal(err)
		}
		outside := filepath.Join(t.TempDir(), "secret")
		if err := os.WriteFile(outside, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(outside, filepath.Join(s.Dir, "Dockerfile")); err != nil {
			t.Fatal(err)
		}
		r := &fakeRunner{}
		_, err := (&docker.Deployer{Runner: r}).Deploy(context.Background(), s)
		var se *acceptance.StageError
		if !errors.As(err, &se) || !strings.Contains(se.Log, "resolves outside the repository") || r.call("build") != nil {
			t.Fatalf("err = %v, calls = %q", err, r.subcommands())
		}
	})
}

func TestSmoke(t *testing.T) {
	s := spec(t)
	dep := acceptance.Deployment{ID: "foreman-acc-bibzor-demo-api", Spec: s}
	t.Run("ready after a few tries", func(t *testing.T) {
		var mu sync.Mutex
		var urls []string
		probe := func(_ context.Context, url string) (int, error) {
			mu.Lock()
			defer mu.Unlock()
			urls = append(urls, url)
			if len(urls) < 3 {
				return 0, errors.New("connection refused")
			}
			return 200, nil
		}
		d := &docker.Deployer{Runner: &fakeRunner{}, Probe: probe, Interval: time.Millisecond, SmokeTimeout: time.Second}
		if err := d.Smoke(context.Background(), dep, s.Config.Smoke); err != nil {
			t.Fatal(err)
		}
		if len(urls) != 3 || urls[0] != "http://127.0.0.1:18080/healthz" {
			t.Fatalf("probed %q", urls)
		}
	})
	t.Run("timeout", func(t *testing.T) {
		r := &fakeRunner{}
		probe := func(context.Context, string) (int, error) { return 503, nil }
		d := &docker.Deployer{Runner: r, Probe: probe, Interval: time.Millisecond, SmokeTimeout: 20 * time.Millisecond}
		err := d.Smoke(context.Background(), dep, s.Config.Smoke)
		var se *acceptance.StageError
		if !errors.As(err, &se) || se.Stage != "smoke" || !strings.Contains(se.Log, "GET /healthz: want 200, got status 503") {
			t.Fatalf("err = %#v", err)
		}
		if logs := r.call("logs"); !slices.Equal(logs, []string{"logs", "--tail", "100", "foreman-acc-bibzor-demo-api"}) {
			t.Errorf("logs = %q", logs)
		}
	})
}

func TestStop(t *testing.T) {
	r := &fakeRunner{}
	if err := (&docker.Deployer{Runner: r}).Stop(context.Background(), acceptance.Deployment{ID: "foreman-acc-x"}); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(r.calls[0], []string{"rm", "-f", "foreman-acc-x"}) {
		t.Fatalf("calls = %q", r.calls)
	}
}
