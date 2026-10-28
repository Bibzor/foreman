package workspace

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Bibzor/foreman/internal/config"
	"github.com/Bibzor/foreman/internal/github"
	"github.com/Bibzor/foreman/internal/skillpack"
	"github.com/Bibzor/foreman/skills"
)

const testToken = "ghs_TESTtokenNEVERonDISK"

type fakeTokens struct{ branch string }

func (fakeTokens) Token(context.Context, config.Repo) (github.Secret, error) {
	return github.NewSecret(testToken), nil
}

func (f fakeTokens) DefaultBranch(context.Context, config.Repo) (string, error) {
	return f.branch, nil
}

func run(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-c", "user.name=t", "-c", "user.email=t@example.com", "-c", "init.defaultBranch=main"}, args...)...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_CONFIG_NOSYSTEM=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// upstream makes a bare repository with two commits on trunk.
func upstream(t *testing.T) string {
	t.Helper()
	src := t.TempDir()
	run(t, src, "init", "-q", "-b", "trunk")
	if err := os.WriteFile(filepath.Join(src, "AGENTS.md"), []byte("# demo\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	run(t, src, "add", ".")
	run(t, src, "commit", "-q", "-m", "one")
	if err := os.WriteFile(filepath.Join(src, "README.md"), []byte("hi\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	run(t, src, "add", ".")
	run(t, src, "commit", "-q", "-m", "two")
	bare := filepath.Join(t.TempDir(), "demo-api.git")
	run(t, src, "clone", "-q", "--bare", src, bare)
	return bare
}

func newWorkspace(t *testing.T, bare string) *Workspace {
	return &Workspace{
		DataDir:  t.TempDir(),
		Tokens:   fakeTokens{branch: "trunk"},
		Skills:   skills.FS,
		Version:  "v1.2.3",
		CloneURL: func(config.Repo) string { return "file://" + bare },
	}
}

func TestPrepare(t *testing.T) {
	bare := upstream(t)
	w := newWorkspace(t, bare)
	repo := config.Repo{Owner: "Bibzor", Name: "demo-api"}

	// A leftover from a previous run must disappear.
	stale := filepath.Join(w.DataDir, "work", "Bibzor", "demo-api", "7", "stale.txt")
	if err := os.MkdirAll(filepath.Dir(stale), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(stale, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	d, err := w.Prepare(context.Background(), repo, 7)
	if err != nil {
		t.Fatal(err)
	}
	if d.Workdir != filepath.Join(w.DataDir, "work", "Bibzor", "demo-api", "7") ||
		d.SkillsDir != filepath.Join(w.DataDir, "skills", "Bibzor", "demo-api", "7") {
		t.Fatalf("dirs = %+v", d)
	}
	if _, err := os.Stat(stale); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("workspace is not fresh")
	}
	if _, err := os.Stat(filepath.Join(d.Workdir, "AGENTS.md")); err != nil {
		t.Fatal(err)
	}
	if n := run(t, d.Workdir, "rev-list", "--count", "HEAD"); n != "1" {
		t.Errorf("clone has %s commits, want a shallow clone", n)
	}
	if u := run(t, d.Workdir, "remote", "get-url", "origin"); u != "file://"+bare {
		t.Errorf("remote = %q", u)
	}
	cfg, err := os.ReadFile(filepath.Join(d.Workdir, ".git", "config"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(strings.ToLower(string(cfg)), "extraheader") || strings.Contains(string(cfg), testToken) {
		t.Errorf(".git/config carries a credential:\n%s", cfg)
	}
	if err := scanForSecrets(d.Workdir, []string{testToken}); err != nil {
		t.Error(err)
	}

	// Skills: verified, outside the clone, loadable as a plugin.
	if _, err := os.Stat(filepath.Join(d.Workdir, ".claude")); !errors.Is(err, os.ErrNotExist) {
		t.Error("skills were installed in the clone")
	}
	r, err := skillpack.Verify(skills.FS, d.SkillsDir, "v1.2.3")
	if err != nil || !r.OK() {
		t.Fatalf("verify: %+v %v", r, err)
	}
	b, err := os.ReadFile(filepath.Join(d.SkillsDir, ".claude-plugin", "plugin.json"))
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]string
	if err := json.Unmarshal(b, &m); err != nil || m["name"] != "foreman" || m["skills"] != "./.claude/skills" {
		t.Fatalf("plugin.json = %s", b)
	}
}

func TestCloneFailureRedactsAndCleansUp(t *testing.T) {
	w := newWorkspace(t, filepath.Join(t.TempDir(), "missing.git"))
	_, err := w.Prepare(context.Background(), config.Repo{Owner: "o", Name: "r"}, 1)
	if err == nil {
		t.Fatal("clone of a missing repo succeeded")
	}
	if strings.Contains(err.Error(), testToken) {
		t.Fatalf("token in error: %v", err)
	}
	if _, serr := os.Stat(filepath.Join(w.DataDir, "work", "o", "r", "1")); !errors.Is(serr, os.ErrNotExist) {
		t.Fatal("half-made clone left behind")
	}
}

func TestGitSeesTheHeaderOnlyThroughEnv(t *testing.T) {
	dir := t.TempDir()
	run(t, dir, "init", "-q")
	auth := []string{"GIT_CONFIG_COUNT=1", "GIT_CONFIG_KEY_0=http.extraHeader", "GIT_CONFIG_VALUE_0=Authorization: Basic xyz"}
	cmd := exec.Command("git", "config", "--get", "http.extraHeader")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), auth...)
	out, err := cmd.Output()
	if err != nil || strings.TrimSpace(string(out)) != "Authorization: Basic xyz" {
		t.Fatalf("git does not read GIT_CONFIG_*: %q %v", out, err)
	}
	if err := scanForSecrets(filepath.Join(dir, ".git"), []string{"xyz"}); err != nil {
		t.Fatal(err)
	}
}

func TestScanForSecrets(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "refs"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "refs", "x"), []byte("url = https://x-access-token:"+testToken+"@github.com"), 0o600); err != nil {
		t.Fatal(err)
	}
	err := scanForSecrets(dir, []string{testToken})
	if !errors.Is(err, ErrTokenOnDisk) || strings.Contains(err.Error(), testToken) {
		t.Fatalf("err = %v", err)
	}
}
