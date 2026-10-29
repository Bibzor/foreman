package develop

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/Bibzor/foreman/internal/project"
	"github.com/Bibzor/foreman/internal/workspace"
)

// The deterministic checks, by the name the issue comment gives them.
const (
	CheckCommitOrder = "commit-order"
	CheckProtected   = "protected-paths"
	CheckDiffSize    = "diff-size"
	CheckTest        = "test"
	CheckLint        = "lint"
)

// CheckError is a failed deterministic check. Detail may quote output of
// code the agent wrote: it is untrusted.
type CheckError struct {
	Check  string
	Detail string
}

func (e *CheckError) Error() string { return "check " + e.Check + " failed: " + e.Detail }

func fail(check, format string, args ...any) error {
	return &CheckError{Check: check, Detail: fmt.Sprintf(format, args...)}
}

// Range is the history the checks look at, in a clone foreman owns.
type Range struct {
	Base string // where the work started, on the default branch
	// DevTip is the tip pushed by the development run: the commits after
	// it are revision commits. Empty during the development run.
	DevTip string
	// Head is the tip foreman pushed last: the commits after it are the
	// ones of the current run. Empty during the development run.
	Head string
	Tip  string
}

// TestChange is a test file changed by a revision's test: commit.
type TestChange struct {
	Path   string
	Status string // added, modified or deleted
}

type commit struct {
	sha, subject string
	files        []fileChange
}

type fileChange struct{ status, path string }

func isTestCommit(subject string) bool { return strings.HasPrefix(subject, "test:") }

// CheckHistory runs the checks that need no code to run: commit-order,
// protected-paths and diff-size. It returns the test files changed by the
// test: commits after r.Head, for the reviewer.
func CheckHistory(ctx context.Context, dir string, p project.Config, r Range) ([]TestChange, error) {
	changes, err := commitOrder(ctx, dir, p, r)
	if err != nil {
		return nil, err
	}
	if err := protectedPaths(ctx, dir, p, r); err != nil {
		return nil, err
	}
	if err := diffSize(ctx, dir, p, r); err != nil {
		return nil, err
	}
	return changes, nil
}

func commits(ctx context.Context, dir string, r Range) ([]commit, error) {
	if _, err := workspace.Git(ctx, dir, "merge-base", "--is-ancestor", r.Base, r.Tip); err != nil {
		return nil, fail(CheckCommitOrder, "the branch does not start from %s", short(r.Base))
	}
	out, err := workspace.Git(ctx, dir, "rev-list", "--reverse", "--parents", r.Base+".."+r.Tip)
	if err != nil {
		return nil, err
	}
	var cs []commit
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) == 0 {
			continue
		}
		if len(f) > 2 {
			return nil, fail(CheckCommitOrder, "merge commit %s: the history must be linear", short(f[0]))
		}
		c := commit{sha: f[0]}
		if c.subject, err = workspace.Git(ctx, dir, "log", "-1", "--format=%s", c.sha); err != nil {
			return nil, err
		}
		files, err := workspace.Git(ctx, dir, "diff-tree", "--no-commit-id", "--no-renames", "-r", "--name-status", c.sha)
		if err != nil {
			return nil, err
		}
		for _, fl := range strings.Split(files, "\n") {
			status, path, ok := strings.Cut(fl, "\t")
			if ok {
				c.files = append(c.files, fileChange{status: status, path: path})
			}
		}
		cs = append(cs, c)
	}
	return cs, nil
}

// index returns the position of sha in cs, -1 for the base, or an error
// if the commit is not on the branch any more: pushed history was
// rewritten.
func index(cs []commit, sha, base, what string) (int, error) {
	if sha == base {
		return -1, nil
	}
	for i, c := range cs {
		if c.sha == sha {
			return i, nil
		}
	}
	return 0, fail(CheckCommitOrder, "the %s %s is not on the branch any more: pushed commits were rewritten", what, short(sha))
}

func commitOrder(ctx context.Context, dir string, p project.Config, r Range) ([]TestChange, error) {
	cs, err := commits(ctx, dir, r)
	if err != nil {
		return nil, err
	}
	if len(cs) == 0 {
		return nil, fail(CheckCommitOrder, "the branch has no commit")
	}
	devEnd, headEnd := len(cs)-1, len(cs)-1
	if r.DevTip != "" {
		if devEnd, err = index(cs, r.DevTip, r.Base, "development tip"); err != nil {
			return nil, err
		}
	}
	if r.Head != "" {
		if headEnd, err = index(cs, r.Head, r.Base, "last pushed commit"); err != nil {
			return nil, err
		}
	}

	if !isTestCommit(cs[0].subject) {
		return nil, fail(CheckCommitOrder, "the first commit must be a `test:` commit with the tests alone, not %q", cs[0].subject)
	}
	block := map[string]bool{}
	i := 0
	for ; i < len(cs) && i <= devEnd && isTestCommit(cs[i].subject); i++ {
		for _, f := range cs[i].files {
			if !p.IsTestPath(f.path) {
				return nil, fail(CheckCommitOrder, "test block touches non-test file %s", f.path)
			}
			block[f.path] = true
		}
	}
	for ; i <= devEnd; i++ {
		for _, f := range cs[i].files {
			if block[f.path] || p.IsTestPath(f.path) {
				return nil, fail(CheckCommitOrder, "commit %s %q touches test file %s after the test commits",
					short(cs[i].sha), cs[i].subject, f.path)
			}
		}
	}
	var changes []TestChange
	for ; i < len(cs); i++ {
		test := isTestCommit(cs[i].subject)
		for _, f := range cs[i].files {
			switch {
			case test && !p.IsTestPath(f.path):
				return nil, fail(CheckCommitOrder, "revision test commit %s touches non-test file %s", short(cs[i].sha), f.path)
			case !test && p.IsTestPath(f.path):
				return nil, fail(CheckCommitOrder, "commit %s %q touches test file %s: tests change only in `test:` commits",
					short(cs[i].sha), cs[i].subject, f.path)
			case test && i > headEnd:
				changes = append(changes, TestChange{Path: f.path, Status: statusName(f.status)})
			}
		}
	}
	return changes, nil
}

func statusName(s string) string {
	switch s {
	case "A":
		return "added"
	case "D":
		return "deleted"
	}
	return "modified"
}

func protectedPaths(ctx context.Context, dir string, p project.Config, r Range) error {
	out, err := workspace.Git(ctx, dir, "diff", "--no-renames", "--name-only", r.Base, r.Tip)
	if err != nil {
		return err
	}
	for _, path := range strings.Split(out, "\n") {
		if path != "" && p.IsProtected(path) {
			return fail(CheckProtected, "%s is protected", path)
		}
	}
	return nil
}

// DiffLines counts the added and deleted lines between base and tip; a
// binary file counts as one line.
func DiffLines(ctx context.Context, dir, base, tip string) (int, error) {
	out, err := workspace.Git(ctx, dir, "diff", "--no-renames", "--numstat", base, tip)
	if err != nil {
		return 0, err
	}
	total := 0
	for _, line := range strings.Split(out, "\n") {
		f := strings.SplitN(line, "\t", 3)
		if len(f) < 3 {
			continue
		}
		if f[0] == "-" {
			total++
			continue
		}
		add, err1 := strconv.Atoi(f[0])
		del, err2 := strconv.Atoi(f[1])
		if err := errors.Join(err1, err2); err != nil {
			return 0, err
		}
		total += add + del
	}
	return total, nil
}

func diffSize(ctx context.Context, dir string, p project.Config, r Range) error {
	n, err := DiffLines(ctx, dir, r.Base, r.Tip)
	if err != nil {
		return err
	}
	if n > p.MaxDiffLines {
		return fail(CheckDiffSize, "the diff changes %d lines, the limit is %d", n, p.MaxDiffLines)
	}
	return nil
}

// ChangedFiles lists the files that differ between base and tip.
func ChangedFiles(ctx context.Context, dir, base, tip string) ([]string, error) {
	out, err := workspace.Git(ctx, dir, "diff", "--no-renames", "--name-only", base, tip)
	if err != nil || out == "" {
		return nil, err
	}
	return strings.Split(out, "\n"), nil
}

// CommandTimeout bounds each project command foreman runs.
const CommandTimeout = 15 * time.Minute

// RunCommands runs the project's test command, then its lint command, in
// dir. They run as argv, never through a shell, with an environment that
// carries no credential.
//
// They execute code the agent wrote, on the host, as the local user.
func RunCommands(ctx context.Context, dir string, p project.Config) error {
	for _, c := range []struct {
		check string
		cmd   project.Command
	}{{CheckTest, p.Test}, {CheckLint, p.Lint}} {
		if err := runCommand(ctx, dir, c.check, c.cmd); err != nil {
			return err
		}
	}
	return nil
}

func runCommand(ctx context.Context, dir, check string, c project.Command) error {
	ctx, cancel := context.WithTimeout(ctx, CommandTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, c.Argv[0], c.Argv[1:]...) //nolint:gosec // argv validated by project.Parse, no shell
	cmd.Dir = dir
	cmd.Env = commandEnv(os.Environ())
	cmd.WaitDelay = 5 * time.Second
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	err := cmd.Run()
	switch {
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		return fail(check, "`%s` timed out after %s", c, CommandTimeout)
	case err != nil:
		return fail(check, "`%s`: %v\n%s", c, err, tail(out.String(), 3000))
	}
	return nil
}

// commandEnv keeps what a build needs from environ: nothing that could
// carry a credential.
func commandEnv(environ []string) []string {
	var out []string
	for _, kv := range environ {
		name, _, _ := strings.Cut(kv, "=")
		switch name {
		case "PATH", "HOME", "USER", "LOGNAME", "LANG", "TMPDIR", "TERM":
			out = append(out, kv)
		default:
			if strings.HasPrefix(name, "LC_") {
				out = append(out, kv)
			}
		}
	}
	return out
}

func tail(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) > n {
		s = "..." + strings.ToValidUTF8(s[len(s)-n:], "")
	}
	return s
}

func short(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}
