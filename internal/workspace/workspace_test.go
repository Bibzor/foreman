package workspace

import (
	"context"
	"encoding/base64"
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

var repoID = config.Repo{Owner: "Bibzor", Name: "demo-api"}

func write(t *testing.T, dir, name, content string) {
	t.Helper()
	p := filepath.Join(dir, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

// commit writes files in dir and commits them, as an agent would.
func commit(t *testing.T, dir, msg string, files map[string]string) {
	t.Helper()
	for name, content := range files {
		write(t, dir, name, content)
	}
	run(t, dir, "add", "-A")
	run(t, dir, "commit", "-q", "-m", msg)
}

func TestPrepareReadsProjectFile(t *testing.T) {
	bare := upstream(t)
	w := newWorkspace(t, bare)
	d, err := w.Prepare(context.Background(), repoID, 7)
	if err != nil {
		t.Fatal(err)
	}
	if d.ProjectFile != nil || d.BaseBranch != "trunk" || d.Base != run(t, d.Workdir, "rev-parse", "HEAD") {
		t.Fatalf("dirs = %+v", d)
	}

	src := t.TempDir()
	run(t, src, "clone", "-q", bare, ".")
	commit(t, src, "config", map[string]string{".foreman.yml": "agent: claude\n"})
	run(t, src, "push", "-q", "origin", "HEAD:trunk")
	d, err = w.PrepareDev(context.Background(), repoID, 7, "foreman/7")
	if err != nil {
		t.Fatal(err)
	}
	if string(d.ProjectFile) != "agent: claude\n" {
		t.Fatalf("ProjectFile = %q", d.ProjectFile)
	}
	if b := run(t, d.Workdir, "branch", "--show-current"); b != "foreman/7" {
		t.Fatalf("branch = %q", b)
	}
}

func TestPrepareAt(t *testing.T) {
	bare := upstream(t)
	src := t.TempDir()
	run(t, src, "clone", "-q", bare, ".")
	commit(t, src, "merge", map[string]string{".foreman.yml": "at the merge\n", "app.go": "package app\n"})
	merge := run(t, src, "rev-parse", "HEAD")
	// The default branch moves on after the merge.
	commit(t, src, "later", map[string]string{".foreman.yml": "later\n", "app.go": "package app // later\n"})
	run(t, src, "push", "-q", "origin", "HEAD:trunk")

	w := newWorkspace(t, bare)
	d, err := w.PrepareAt(context.Background(), repoID, 7, merge)
	if err != nil {
		t.Fatal(err)
	}
	if got := run(t, d.Workdir, "rev-parse", "HEAD"); got != merge || d.Base != merge {
		t.Fatalf("HEAD = %s, Base = %s, want the merge commit %s", got, d.Base, merge)
	}
	if string(d.ProjectFile) != "at the merge\n" || d.BaseBranch != "trunk" {
		t.Fatalf("dirs = %+v, want .foreman.yml of the merge commit", d)
	}
	if b, _ := os.ReadFile(filepath.Join(d.Workdir, "app.go")); string(b) != "package app\n" {
		t.Fatalf("app.go = %q", b)
	}
	if err := scanForSecrets(d.Workdir, []string{testToken}); err != nil {
		t.Fatal(err)
	}
	// The tip itself needs no fetch.
	tip := run(t, src, "rev-parse", "HEAD")
	if d, err := w.PrepareAt(context.Background(), repoID, 7, tip); err != nil || d.Base != tip || string(d.ProjectFile) != "later\n" {
		t.Fatalf("at the tip: %+v %v", d, err)
	}
}

// pushBranch makes foreman/7 on the upstream: two commits over trunk.
func pushBranch(t *testing.T, bare string) (base, tip string) {
	t.Helper()
	src := t.TempDir()
	run(t, src, "clone", "-q", bare, ".")
	base = run(t, src, "rev-parse", "HEAD")
	run(t, src, "checkout", "-q", "-b", "foreman/7")
	commit(t, src, "test: a", map[string]string{"a_test.go": "package a\n"})
	commit(t, src, "feat: a", map[string]string{"a.go": "package a\n"})
	run(t, src, "push", "-q", "origin", "foreman/7")
	return base, run(t, src, "rev-parse", "HEAD")
}

func TestPrepareBranch(t *testing.T) {
	bare := upstream(t)
	base, tip := pushBranch(t, bare)
	// The default branch moves on after the branch was made.
	src := t.TempDir()
	run(t, src, "clone", "-q", bare, ".")
	commit(t, src, "config", map[string]string{".foreman.yml": "on trunk\n"})
	run(t, src, "push", "-q", "origin", "HEAD:trunk")

	w := newWorkspace(t, bare)
	d, err := w.PrepareBranch(context.Background(), repoID, 7, "foreman/7", 3, base)
	if err != nil {
		t.Fatal(err)
	}
	if got := run(t, d.Workdir, "rev-parse", "HEAD"); got != tip {
		t.Fatalf("HEAD = %s, want the branch tip %s", got, tip)
	}
	if string(d.ProjectFile) != "on trunk\n" {
		t.Fatalf("ProjectFile = %q, want the default branch's", d.ProjectFile)
	}
	if err := scanForSecrets(d.Workdir, []string{testToken}); err != nil {
		t.Fatal(err)
	}
	if _, err := w.PrepareBranch(context.Background(), repoID, 7, "foreman/7", 1, base); err == nil ||
		!strings.Contains(err.Error(), "does not reach") {
		t.Fatalf("too shallow: err = %v", err)
	}
}

func TestSnapshotLeavesTheAgentsGitBehind(t *testing.T) {
	bare := upstream(t)
	w := newWorkspace(t, bare)
	d, err := w.PrepareDev(context.Background(), repoID, 7, "foreman/7")
	if err != nil {
		t.Fatal(err)
	}
	commit(t, d.Workdir, "test: a", map[string]string{"a_test.go": "package a\n"})
	// What an agent could leave behind.
	write(t, d.Workdir, "uncommitted.go", "package a\n")
	marker := filepath.Join(t.TempDir(), "hook-ran")
	write(t, d.Workdir, ".git/hooks/post-checkout", "#!/bin/sh\ntouch "+marker+"\n")
	if err := os.Chmod(filepath.Join(d.Workdir, ".git/hooks/post-checkout"), 0o700); err != nil {
		t.Fatal(err)
	}
	run(t, d.Workdir, "config", "url.https://evil.example/.insteadOf", "file://")

	dir, tip, err := w.Snapshot(context.Background(), repoID, 7, d.Workdir, "foreman/7")
	if err != nil {
		t.Fatal(err)
	}
	if tip != run(t, d.Workdir, "rev-parse", "HEAD") {
		t.Fatal("tip differs")
	}
	if _, err := os.Stat(filepath.Join(dir, "uncommitted.go")); !errors.Is(err, os.ErrNotExist) {
		t.Error("uncommitted file crossed over")
	}
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Error("a hook from the agent's clone ran")
	}
	cfg, _ := os.ReadFile(filepath.Join(dir, ".git", "config"))
	if strings.Contains(string(cfg), "evil") {
		t.Errorf("agent config crossed over:\n%s", cfg)
	}
	if _, _, err := w.Snapshot(context.Background(), repoID, 7, d.Workdir, "missing"); err == nil {
		t.Error("snapshot of a missing branch succeeded")
	}
}

// fakeGit puts a git wrapper first in PATH that records its arguments and
// whether it saw the auth header in its environment, then runs git.
func fakeGit(t *testing.T) (log string) {
	t.Helper()
	gitPath, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	log = filepath.Join(t.TempDir(), "git.log")
	script := "#!/bin/sh\n" +
		"echo \"ARGS $*\" >> " + log + "\n" +
		"env | grep '^GIT_CONFIG_VALUE_0=' | sed 's/^/ENV /' >> " + log + "\n" +
		"exec " + gitPath + " \"$@\"\n"
	write(t, bin, "git", script)
	if err := os.Chmod(filepath.Join(bin, "git"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	return log
}

func TestPushTokenOnlyInEnv(t *testing.T) {
	bare := upstream(t)
	w := newWorkspace(t, bare)
	d, err := w.PrepareDev(context.Background(), repoID, 7, "foreman/7")
	if err != nil {
		t.Fatal(err)
	}
	commit(t, d.Workdir, "test: a", map[string]string{"a_test.go": "package a\n"})
	dir, tip, err := w.Snapshot(context.Background(), repoID, 7, d.Workdir, "foreman/7")
	if err != nil {
		t.Fatal(err)
	}

	log := fakeGit(t)
	if err := w.Push(context.Background(), repoID, dir, tip, "foreman/7", true); err != nil {
		t.Fatal(err)
	}
	if got := run(t, bare, "rev-parse", "refs/heads/foreman/7"); got != tip {
		t.Fatalf("upstream branch = %s, want %s", got, tip)
	}
	b, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	var sawPush, sawHeader bool
	for _, line := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(line, "ARGS") && strings.Contains(line, " push ") {
			sawPush = true
			if strings.Contains(line, "Authorization") || strings.Contains(line, "x-access-token") {
				t.Errorf("credential on the command line: %s", line)
			}
		}
		if strings.HasPrefix(line, "ENV GIT_CONFIG_VALUE_0=Authorization: Basic ") {
			sawHeader = true
		}
	}
	if !sawPush || !sawHeader {
		t.Fatalf("push = %v, header in env = %v\n%s", sawPush, sawHeader, b)
	}
	basic := base64.StdEncoding.EncodeToString([]byte("x-access-token:" + testToken))
	for _, root := range []string{w.DataDir, bare} {
		if err := scanForSecrets(root, []string{testToken, basic}); err != nil {
			t.Errorf("%s: %v", root, err)
		}
	}
}

func TestPushFastForwardOnly(t *testing.T) {
	bare := upstream(t)
	pushBranch(t, bare)
	w := newWorkspace(t, bare)
	d, err := w.PrepareDev(context.Background(), repoID, 7, "foreman/7")
	if err != nil {
		t.Fatal(err)
	}
	commit(t, d.Workdir, "test: other", map[string]string{"b_test.go": "package b\n"})
	dir, tip, err := w.Snapshot(context.Background(), repoID, 7, d.Workdir, "foreman/7")
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Push(context.Background(), repoID, dir, tip, "foreman/7", false); err == nil {
		t.Fatal("a non-fast-forward push was accepted")
	}
	if err := w.Push(context.Background(), repoID, dir, tip, "foreman/7", true); err != nil {
		t.Fatalf("forced push: %v", err)
	}
}
