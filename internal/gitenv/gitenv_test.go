package gitenv_test

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Bibzor/foreman/internal/gitenv"
)

func TestEnv(t *testing.T) {
	env := gitenv.Env(gitenv.Pair{Key: "http.extraHeader", Value: "x"})
	want := []string{
		"GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=" + os.DevNull, "GIT_PAGER=cat",
		"GIT_CONFIG_KEY_0=core.fsmonitor", "GIT_CONFIG_VALUE_0=false",
		"GIT_CONFIG_KEY_1=core.hooksPath", "GIT_CONFIG_VALUE_1=" + os.DevNull,
		"GIT_CONFIG_KEY_2=core.pager", "GIT_CONFIG_VALUE_2=cat",
		"GIT_CONFIG_KEY_3=diff.external", "GIT_CONFIG_VALUE_3=true",
		"GIT_CONFIG_KEY_8=http.extraHeader", "GIT_CONFIG_VALUE_8=x",
		"GIT_CONFIG_COUNT=9",
	}
	for _, w := range want {
		found := false
		for _, kv := range env {
			found = found || kv == w
		}
		if !found {
			t.Errorf("missing %s in %q", w, env)
		}
	}
}

// hostile makes a repository whose .git/config runs a command, touching a
// marker, from every option that can: fsmonitor, pager, external diff,
// hooks, editor.
func hostile(t *testing.T) (dir, marker string) {
	t.Helper()
	dir = t.TempDir()
	marker = filepath.Join(t.TempDir(), "ran")
	git(t, dir, nil, "init", "-q")
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("one\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	git(t, dir, nil, "add", "a.txt")
	git(t, dir, nil, "-c", "user.name=t", "-c", "user.email=t@example.com", "commit", "-q", "-m", "one")
	touch := "touch " + marker
	for k, v := range map[string]string{
		"core.fsmonitor": touch,
		"core.pager":     touch,
		"diff.external":  touch,
		"core.editor":    touch,
		"core.hooksPath": filepath.Join(dir, "hooks"),
	} {
		git(t, dir, nil, "config", k, v)
	}
	hook := filepath.Join(dir, "hooks", "pre-commit")
	if err := os.MkdirAll(filepath.Dir(hook), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(hook, []byte("#!/bin/sh\n"+touch+"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("two\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir, marker
}

func git(t *testing.T, dir string, env []string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append([]string{"PATH=" + os.Getenv("PATH"), "HOME=" + t.TempDir(),
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com"}, env...)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func ran(marker string) bool {
	_, err := os.Stat(marker)
	return !errors.Is(err, os.ErrNotExist)
}

// The agent's git commands, and the plumbing foreman runs, against a
// hostile .git/config.
var commands = [][]string{
	{"--no-pager", "status"},
	{"status"},
	{"--no-pager", "diff", "--no-ext-diff"},
	{"diff"},
	{"diff", "--ext-diff"},
	{"--no-pager", "log", "--no-ext-diff", "-p"},
	{"log", "-p", "--ext-diff"},
	{"add", "a.txt"},
	{"commit", "-q", "-a", "-m", "two"},
}

func TestForcedOptionsDefeatAHostileConfig(t *testing.T) {
	// Control: without the forced options, the configuration does run.
	dir, marker := hostile(t)
	git(t, dir, []string{"GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=" + os.DevNull}, "diff")
	if !ran(marker) {
		t.Fatal("control: the hostile diff.external did not run, the test proves nothing")
	}

	for _, args := range commands {
		dir, marker := hostile(t)
		git(t, dir, gitenv.Env(), args...)
		if ran(marker) {
			t.Errorf("git %s ran a command from .git/config", strings.Join(args, " "))
		}
	}
}
