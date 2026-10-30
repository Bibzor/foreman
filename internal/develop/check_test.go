package develop_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/Bibzor/foreman/internal/config"
	"github.com/Bibzor/foreman/internal/develop"
	"github.com/Bibzor/foreman/internal/sandbox"
)

func git(t *testing.T, dir string, args ...string) string {
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

// repo is a work repository with one base commit.
type repo struct {
	t    *testing.T
	dir  string
	base string
}

func newRepo(t *testing.T) *repo {
	t.Helper()
	r := &repo{t: t, dir: t.TempDir()}
	git(t, r.dir, "init", "-q")
	r.commit("init", map[string]string{"AGENTS.md": "# demo\n", "main.go": "package main\n", "main_test.go": "package main\n"})
	r.base = r.head()
	return r
}

func (r *repo) head() string { return git(r.t, r.dir, "rev-parse", "HEAD") }

// commit writes files ("" deletes) and commits them.
func (r *repo) commit(msg string, files map[string]string) string {
	r.t.Helper()
	for name, content := range files {
		p := filepath.Join(r.dir, filepath.FromSlash(name))
		if content == "" {
			if err := os.Remove(p); err != nil {
				r.t.Fatal(err)
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			r.t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			r.t.Fatal(err)
		}
	}
	git(r.t, r.dir, "add", "-A")
	git(r.t, r.dir, "commit", "-q", "--allow-empty", "-m", msg)
	return r.head()
}

// good is a branch that passes every history check.
func (r *repo) good() {
	r.commit("test: version endpoint", map[string]string{"version_test.go": "package main\n// test\n"})
	r.commit("feat: version endpoint", map[string]string{"version.go": "package main\n// code\n"})
}

func checkErr(t *testing.T, err error, check, detail string) {
	t.Helper()
	var ce *develop.CheckError
	if !errors.As(err, &ce) || ce.Check != check || !strings.Contains(ce.Detail, detail) {
		t.Fatalf("err = %v, want check %s with %q", err, check, detail)
	}
}

func TestCheckHistoryDevelopment(t *testing.T) {
	p := testProject(t)
	tests := []struct {
		name          string
		build         func(r *repo)
		check, detail string // empty: passes
	}{
		{name: "tests then code", build: (*repo).good},
		{name: "several test commits", build: func(r *repo) {
			r.commit("test: one", map[string]string{"a_test.go": "x\n"})
			r.commit("test: two", map[string]string{"pkg/b_test.go": "x\n"})
			r.commit("feat: code", map[string]string{"a.go": "x\n"})
		}},
		{name: "no commit", build: func(*repo) {}, check: develop.CheckCommitOrder, detail: "no commit"},
		{name: "code first", build: func(r *repo) {
			r.commit("feat: code", map[string]string{"a.go": "x\n"})
			r.commit("test: late", map[string]string{"a_test.go": "x\n"})
		}, check: develop.CheckCommitOrder, detail: "first commit must be a `test:` commit"},
		{name: "test block touches a non-test file", build: func(r *repo) {
			r.commit("test: and a bit of code", map[string]string{"a_test.go": "x\n", "a.go": "x\n"})
		}, check: develop.CheckCommitOrder, detail: "test block touches non-test file a.go"},
		{name: "test file modified after the test commit", build: func(r *repo) {
			r.good()
			r.commit("fix: make it pass", map[string]string{"version_test.go": "package main\n// weakened\n"})
		}, check: develop.CheckCommitOrder, detail: "touches test file version_test.go"},
		{name: "existing test file modified by production code", build: func(r *repo) {
			r.good()
			r.commit("fix: also", map[string]string{"main_test.go": "package main\n// changed\n"})
		}, check: develop.CheckCommitOrder, detail: "touches test file main_test.go"},
		{name: "new test file added after the code", build: func(r *repo) {
			r.good()
			r.commit("feat: more", map[string]string{"extra_test.go": "x\n"})
		}, check: develop.CheckCommitOrder, detail: "touches test file extra_test.go"},
		{name: "merge commit", build: func(r *repo) {
			r.commit("test: a", map[string]string{"a_test.go": "x\n"})
			git(r.t, r.dir, "checkout", "-q", "-b", "side", r.base)
			r.commit("side", map[string]string{"side.go": "x\n"})
			git(r.t, r.dir, "checkout", "-q", "-")
			git(r.t, r.dir, "merge", "-q", "--no-ff", "-m", "merge", "side")
		}, check: develop.CheckCommitOrder, detail: "merge commit"},
		{name: "protected path", build: func(r *repo) {
			r.good()
			r.commit("chore: ci", map[string]string{".github/workflows/ci.yml": "on: push\n"})
		}, check: develop.CheckProtected, detail: ".github/workflows/ci.yml is protected"},
		{name: "protected file deleted", build: func(r *repo) {
			r.good()
			r.commit("chore: tidy", map[string]string{"AGENTS.md": ""})
		}, check: develop.CheckProtected, detail: "AGENTS.md is protected"},
		{name: "diff too large", build: func(r *repo) {
			r.commit("test: a", map[string]string{"a_test.go": "x\n"})
			r.commit("feat: big", map[string]string{"big.go": strings.Repeat("line\n", 400)})
		}, check: develop.CheckDiffSize, detail: "changes 401 lines, the limit is 400"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := newRepo(t)
			tt.build(r)
			_, err := develop.CheckHistory(context.Background(), r.dir, p, develop.Range{Base: r.base, Tip: r.head()})
			if tt.check == "" {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			checkErr(t, err, tt.check, tt.detail)
		})
	}
}

func TestCheckHistoryRevision(t *testing.T) {
	p := testProject(t)
	tests := []struct {
		name          string
		build         func(r *repo)
		check, detail string
		want          string // the test changes listed, "path:status ..."
	}{
		{name: "code only", build: func(r *repo) {
			r.commit("fix: review", map[string]string{"version.go": "package main\n// fixed\n"})
		}},
		{name: "test commit modifies an existing test", build: func(r *repo) {
			r.commit("test: cover the review", map[string]string{"version_test.go": "package main\n// stricter\n", "new_test.go": "x\n"})
			r.commit("fix: review", map[string]string{"version.go": "package main\n// fixed\n"})
		}, want: "new_test.go:added version_test.go:modified"},
		{name: "test commit touches production code", build: func(r *repo) {
			r.commit("test: and the fix", map[string]string{"version_test.go": "package main\n// x\n", "version.go": "package main\n// x\n"})
		}, check: develop.CheckCommitOrder, detail: "revision test commit"},
		{name: "production commit modifies a test", build: func(r *repo) {
			r.commit("fix: review", map[string]string{"version_test.go": "package main\n// weakened\n"})
		}, check: develop.CheckCommitOrder, detail: "tests change only in `test:` commits"},
		{name: "pushed history rewritten", build: func(r *repo) {
			git(r.t, r.dir, "reset", "-q", "--hard", "HEAD~1")
			r.commit("feat: redone", map[string]string{"version.go": "package main\n// redone\n"})
		}, check: develop.CheckCommitOrder, detail: "pushed commits were rewritten"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := newRepo(t)
			r.good()
			dev := r.head()
			tt.build(r)
			changes, err := develop.CheckHistory(context.Background(), r.dir, p,
				develop.Range{Base: r.base, DevTip: dev, Head: dev, Tip: r.head()})
			if tt.check != "" {
				checkErr(t, err, tt.check, tt.detail)
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			var got []string
			for _, c := range changes {
				got = append(got, c.Path+":"+c.Status)
			}
			if strings.Join(got, " ") != tt.want {
				t.Fatalf("changes = %q, want %q", got, tt.want)
			}
		})
	}
}

// A second revision lists only its own test changes, and the test
// commits of the first revision still pass.
func TestCheckHistorySecondRevision(t *testing.T) {
	r := newRepo(t)
	r.good()
	dev := r.head()
	r.commit("test: first revision", map[string]string{"version_test.go": "package main\n// r1\n"})
	head := r.commit("fix: first revision", map[string]string{"version.go": "package main\n// r1\n"})
	r.commit("test: second revision", map[string]string{"other_test.go": "x\n"})
	changes, err := develop.CheckHistory(context.Background(), r.dir, testProject(t),
		develop.Range{Base: r.base, DevTip: dev, Head: head, Tip: r.head()})
	if err != nil {
		t.Fatal(err)
	}
	if len(changes) != 1 || changes[0].Path != "other_test.go" || changes[0].Status != "added" {
		t.Fatalf("changes = %+v", changes)
	}
}

// fakeSandbox answers Exec with errs[name], writing "output of <name>".
type fakeSandbox struct {
	errs  map[string]error
	execs []string
}

func (f *fakeSandbox) Setup(context.Context, sandbox.Spec, io.Writer) error { return nil }

func (f *fakeSandbox) Exec(_ context.Context, s sandbox.Spec, name string, out io.Writer) error {
	f.execs = append(f.execs, s.Workspace+":"+name)
	_, _ = io.WriteString(out, "output of "+name)
	return f.errs[name]
}

func (f *fakeSandbox) Cleanup(context.Context, config.Repo, int) error { return nil }

func commandSpec() sandbox.Spec {
	return sandbox.Spec{Workspace: "/data/check/o/r/7", TimeoutS: 900, Commands: []sandbox.Command{
		{Name: "test", Argv: []string{"make", "test"}}, {Name: "lint", Argv: []string{"make", "lint"}},
	}}
}

func TestRunCommands(t *testing.T) {
	ctx := context.Background()
	sb := &fakeSandbox{}
	if err := develop.RunCommands(ctx, sb, commandSpec()); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(sb.execs, []string{"/data/check/o/r/7:test", "/data/check/o/r/7:lint"}) {
		t.Fatalf("execs = %q", sb.execs)
	}
	checkErr(t, develop.RunCommands(ctx, &fakeSandbox{errs: map[string]error{"test": &sandbox.ExitError{Code: 2}}}, commandSpec()),
		develop.CheckTest, "`make test`: exit status 2\noutput of test")
	checkErr(t, develop.RunCommands(ctx, &fakeSandbox{errs: map[string]error{"lint": &sandbox.ExitError{Code: 1}}}, commandSpec()),
		develop.CheckLint, "`make lint`: exit status 1")
	checkErr(t, develop.RunCommands(ctx, &fakeSandbox{errs: map[string]error{"test": fmt.Errorf("%w after 15m0s", sandbox.ErrTimeout)}}, commandSpec()),
		develop.CheckTest, "`make test` timed out after 15m0s")
	// A sandbox that cannot run is not the agent's failure.
	err := develop.RunCommands(ctx, &fakeSandbox{errs: map[string]error{"test": errors.New("docker run: no such image")}}, commandSpec())
	var ce *develop.CheckError
	if err == nil || errors.As(err, &ce) || !strings.Contains(err.Error(), "no such image") {
		t.Fatalf("err = %v", err)
	}
}
