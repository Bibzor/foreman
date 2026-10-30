//go:build manual

// Manual proofs against a real Docker daemon, for the pull request:
//
//	go test -tags manual -run Manual -v ./internal/sandbox/docker/
//
// FOREMAN_MANUAL_KEY_PATH may name the real GitHub App key: the sandbox
// only tests that the path exists, never reads it. Without it, a decoy
// key under $HOME stands in.
package docker_test

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Bibzor/foreman/internal/sandbox"
	"github.com/Bibzor/foreman/internal/sandbox/docker"
)

const image = "golang:1.27"

const probeTest = `package probe

import (
	"net/http"
	"os"
	"testing"
	"time"
)

func TestHTTP(t *testing.T) {
	c := &http.Client{Timeout: 10 * time.Second}
	resp, err := c.Get("https://proxy.golang.org/")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	resp.Body.Close()
	t.Logf("GET: %s", resp.Status)
}

func TestWriteGitConfig(t *testing.T) {
	f, err := os.OpenFile("/work/.git/config", os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatalf("open .git/config: %v", err)
	}
	defer f.Close()
	if _, err := f.WriteString("[filter \"x\"]\n\tclean = touch /tmp/pwned\n"); err != nil {
		t.Fatalf("write .git/config: %v", err)
	}
}

func TestWriteWorkspace(t *testing.T) {
	if err := os.WriteFile("/work/written.txt", []byte("ok\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
}

func TestReadHost(t *testing.T) {
	for _, p := range []string{os.Getenv("CANARY"), os.Getenv("KEY")} {
		b, err := os.ReadFile(p)
		if err != nil {
			t.Errorf("read %s: %v", p, err)
			continue
		}
		t.Errorf("read %s: %d bytes", p, len(b)) // never the content
	}
}
`

type manual struct {
	t                       *testing.T
	data, work, key, canary string
	sb                      *docker.Sandbox
}

func newManual(t *testing.T) *manual {
	t.Helper()
	data := t.TempDir()
	m := &manual{t: t, data: data, sb: &docker.Sandbox{Runner: docker.Exec{}},
		work: filepath.Join(data, "work", "Bibzor", "demo-api", "7")}
	m.canary = filepath.Join(t.TempDir(), "canary.txt")
	write(t, m.canary, "CANARY-outside-the-workspace\n")
	m.key = os.Getenv("FOREMAN_MANUAL_KEY_PATH")
	if m.key == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			t.Fatal(err)
		}
		dir, err := os.MkdirTemp(home, ".foreman-manual-key-")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.RemoveAll(dir) })
		m.key = filepath.Join(dir, "app.pem")
		write(t, m.key, "-----BEGIN RSA PRIVATE KEY-----\ndecoy\n-----END RSA PRIVATE KEY-----\n")
	}
	if _, err := os.Stat(m.key); err != nil {
		t.Fatalf("key: %v", err)
	}
	write(t, filepath.Join(m.work, "go.mod"), "module probe\n\ngo 1.27\n")
	write(t, filepath.Join(m.work, "probe_test.go"), probeTest)
	git := exec.Command("git", "init", "-q", m.work)
	if out, err := git.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v %s", err, out)
	}
	return m
}

func write(t *testing.T, p, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

// spec is a sandbox on the workspace with one command per name.
func (m *manual) spec(cmds map[string][]string) sandbox.Spec {
	s := sandbox.Spec{
		Version: 1, Repo: "Bibzor/demo-api", Issue: 7, Workspace: m.work, Image: image,
		// /go/pkg, not only /go/pkg/mod: go also writes the checksum
		// database it verified in /go/pkg/sumdb.
		Caches: sandbox.CacheDirs(m.data, repo, 7, []string{"/go/pkg"}),
		Env:    map[string]string{"CANARY": m.canary, "KEY": m.key, "GOFLAGS": "-mod=mod"},
		User:   userID(), Limits: sandbox.DefaultLimits, TimeoutS: 300,
	}
	for name, argv := range cmds {
		s.Commands = append(s.Commands, sandbox.Command{Name: name, Argv: argv})
	}
	return s
}

func userID() string { return strconv.Itoa(os.Getuid()) + ":" + strconv.Itoa(os.Getgid()) }

// exec runs name in the sandbox and logs the docker command line and the
// output, for the pull request.
func (m *manual) exec(s sandbox.Spec, name string) (string, error) {
	m.t.Helper()
	c, _ := s.Command(name)
	m.t.Logf("$ docker %s", strings.Join(docker.ExecArgv(s, c, "<name>"), " "))
	var out bytes.Buffer
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	err := m.sb.Exec(ctx, s, name, &out)
	m.t.Logf("exec %s -> err=%v\n%s", strings.Join(c.Argv, " "), err, strings.TrimSpace(out.String()))
	return out.String(), err
}

func failed(err error) bool {
	var ee *sandbox.ExitError
	return errors.As(err, &ee)
}

func TestManualIsolation(t *testing.T) {
	m := newManual(t)
	if err := sandbox.ResetCaches(m.data, repo, 7, m.spec(nil).Caches); err != nil {
		t.Fatal(err)
	}
	gitConfig, err := os.ReadFile(filepath.Join(m.work, ".git", "config"))
	if err != nil {
		t.Fatal(err)
	}

	// Setup has the network and fills the cache.
	setup := m.spec(map[string][]string{
		"setup-0": {"go", "mod", "download", "-x", "golang.org/x/text@v0.21.0"},
		"setup-1": {"go", "test", "-count=1", "-run", "TestHTTP", "-v", "./..."},
	})
	var out bytes.Buffer
	if err := m.sb.Setup(context.Background(), setup, &out); err != nil {
		t.Fatalf("setup: %v\n%s", err, out.String())
	}
	t.Logf("setup (network): ok\n%s", lastLines(out.String(), 8))

	s := m.spec(map[string][]string{
		"0":    {"cat", m.canary},
		"1":    {"test", "-e", m.key},
		"2":    {"go", "test", "-count=1", "-run", "TestReadHost", "-v", "./..."},
		"3":    {"go", "test", "-count=1", "-run", "TestHTTP", "-v", "./..."},
		"4":    {"go", "test", "-count=1", "-run", "TestWriteGitConfig", "-v", "./..."},
		"5":    {"go", "test", "-count=1", "-run", "TestWriteWorkspace", "-v", "./..."},
		"6":    {"id"},
		"7":    {"touch", "/go/pkg/mod/written"},
		"8":    {"go", "mod", "download", "-x", "golang.org/x/text@v0.21.0"},
		"9":    {"touch", "/etc/written"},
		"test": {"go", "test", "-count=1", "-run", "TestWriteWorkspace", "./..."},
	})

	// 1. Neither a canary outside the workspace nor the App key exists in
	// the sandbox.
	if _, err := m.exec(s, "0"); !failed(err) {
		t.Errorf("cat canary: %v", err)
	}
	if _, err := m.exec(s, "1"); !failed(err) {
		t.Errorf("test -e key: %v", err)
	}
	if o, err := m.exec(s, "2"); !failed(err) || strings.Contains(o, "CANARY-outside") {
		t.Errorf("Go test reading the host: %v", err)
	}
	// 2. No network.
	if o, err := m.exec(s, "3"); !failed(err) || !strings.Contains(o, "GET:") {
		t.Errorf("HTTP from a test: %v", err)
	}
	// 3. .git is read-only; the rest of the workspace is not.
	if o, err := m.exec(s, "4"); !failed(err) || !strings.Contains(o, "read-only file system") {
		t.Errorf("write .git/config: %v", err)
	}
	if after, _ := os.ReadFile(filepath.Join(m.work, ".git", "config")); !bytes.Equal(after, gitConfig) {
		t.Errorf(".git/config changed on the host:\n%s", after)
	}
	if _, err := m.exec(s, "5"); err != nil {
		t.Errorf("write the workspace: %v", err)
	}
	// Not root; the cache and the root filesystem are read-only; the cache
	// filled by setup works offline.
	if o, err := m.exec(s, "6"); err != nil || strings.Contains(o, "uid=0(") {
		t.Errorf("id: %q %v", o, err)
	}
	if o, err := m.exec(s, "7"); !failed(err) || !strings.Contains(o, "Read-only file system") {
		t.Errorf("write the cache: %v", err)
	}
	if _, err := m.exec(s, "8"); err != nil {
		t.Errorf("offline download from the cache: %v", err)
	}
	if o, err := m.exec(s, "9"); !failed(err) || !strings.Contains(o, "Read-only file system") {
		t.Errorf("write /etc: %v", err)
	}
	if _, err := m.exec(s, "test"); err != nil {
		t.Errorf("go test: %v", err)
	}
}

// TestManualForemanSandboxExec runs the real binary, as the agent does.
func TestManualForemanSandboxExec(t *testing.T) {
	m := newManual(t)
	if err := sandbox.ResetCaches(m.data, repo, 7, m.spec(nil).Caches); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(t.TempDir(), "foreman")
	if out, err := exec.Command("go", "build", "-o", bin, "github.com/Bibzor/foreman/cmd/foreman").CombinedOutput(); err != nil {
		t.Fatalf("build: %v %s", err, out)
	}
	skills := filepath.Join(m.data, "skills", "Bibzor", "demo-api", "7")
	if err := os.MkdirAll(skills, 0o700); err != nil {
		t.Fatal(err)
	}
	spec, err := sandbox.Write(skills, m.spec(map[string][]string{
		"test": {"go", "test", "-count=1", "-run", "TestWriteWorkspace", "-v", "./..."},
		"lint": {"go", "vet", "./..."},
	}))
	if err != nil {
		t.Fatal(err)
	}
	run := func(specEnv string, args ...string) (string, int) {
		cmd := exec.Command(bin, args...)
		cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + os.Getenv("HOME"), sandbox.EnvSpec + "=" + specEnv}
		out, err := cmd.CombinedOutput()
		code := 0
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			code = ee.ExitCode()
		}
		t.Logf("$ %s=%s foreman %s -> exit %d\n%s", sandbox.EnvSpec, specEnv, strings.Join(args, " "), code, strings.TrimSpace(string(out)))
		return string(out), code
	}
	if out, code := run(spec, "sandbox", "exec", "--data-dir", m.data, "test"); code != 0 || !strings.Contains(out, "PASS") {
		t.Errorf("sandbox exec test: exit %d", code)
	}
	evil := filepath.Join(os.TempDir(), "evil.json")
	b, _ := os.ReadFile(spec)
	write(t, evil, strings.Replace(string(b), m.work, filepath.Dir(filepath.Dir(filepath.Dir(filepath.Dir(m.data)))), 1))
	t.Cleanup(func() { _ = os.Remove(evil) })
	if out, code := run(evil, "sandbox", "exec", "--data-dir", m.data, "test"); code == 0 || !strings.Contains(out, "is not under") {
		t.Errorf("evil spec: exit %d", code)
	}
}

func lastLines(s string, n int) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}
