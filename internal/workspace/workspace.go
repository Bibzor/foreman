// Package workspace prepares what an agent works on: a fresh shallow
// clone with no credential in it, and a verified copy of the skills
// outside the clone.
//
// The installation token reaches the git child process only, as an HTTP
// header passed through GIT_CONFIG_* environment variables: never in the
// command line, the remote URL or .git/config. After the clone, .git is
// scanned for the token and the workspace is discarded if it is found.
//
// After an agent has worked in a clone, foreman never runs git there
// again: Snapshot clones the branch into a directory foreman owns, so
// nothing the agent wrote in .git (configuration, hooks, alternates) and
// no uncommitted file reaches the checks or the push.
//
// The skills are not installed in the clone: Claude Code ignores a
// repository's .claude directory under --restricted, and a plugin
// directory inside the clone would let the repository add hooks or MCP
// servers. They go to a sibling directory loaded with --plugin-dir.
package workspace

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/Bibzor/foreman/internal/config"
	"github.com/Bibzor/foreman/internal/github"
	"github.com/Bibzor/foreman/internal/project"
	"github.com/Bibzor/foreman/internal/skillpack"
)

// Tokens gives clone access to a repository.
type Tokens interface {
	Token(ctx context.Context, repo config.Repo) (github.Secret, error)
	DefaultBranch(ctx context.Context, repo config.Repo) (string, error)
}

// Dirs is a prepared workspace.
type Dirs struct {
	Workdir   string // the clone
	SkillsDir string // the verified skills, a Claude Code plugin
	// BaseBranch is the default branch and Base the commit cloned from it.
	BaseBranch string
	Base       string
	// ProjectFile is .foreman.yml at ProjectSHA, the tip of the default
	// branch when cloned (which is Base, except for PrepareAt), nil when
	// there is none. It is read before any branch is checked out.
	ProjectFile []byte
	ProjectSHA  string
}

// Workspace prepares workspaces under DataDir.
type Workspace struct {
	DataDir string
	Tokens  Tokens
	Skills  fs.FS  // the skills pack
	Version string // the skills version
	// CloneURL returns the URL to clone; nil means https://github.com.
	CloneURL func(config.Repo) string
}

const agentName = "claude"

// Prepare makes a fresh workspace for an issue:
// data_dir/work/<owner>/<repo>/<issue> for the clone and
// data_dir/skills/<owner>/<repo>/<issue> for the skills.
func (w *Workspace) Prepare(ctx context.Context, repo config.Repo, number int) (Dirs, error) {
	rel := filepath.Join(repo.Owner, repo.Name, strconv.Itoa(number))
	d := Dirs{
		Workdir:   filepath.Join(w.DataDir, "work", rel),
		SkillsDir: filepath.Join(w.DataDir, "skills", rel),
	}
	branch, err := w.clone(ctx, repo, d.Workdir)
	if err != nil {
		_ = os.RemoveAll(d.Workdir) // never leave a half-made clone
		return Dirs{}, err
	}
	d.BaseBranch = branch
	if d.Base, err = Git(ctx, d.Workdir, "rev-parse", "HEAD"); err != nil {
		return Dirs{}, err
	}
	if d.ProjectFile, err = projectFile(ctx, d.Workdir); err != nil {
		return Dirs{}, err
	}
	d.ProjectSHA = d.Base
	if err := w.skills(d.SkillsDir); err != nil {
		return Dirs{}, err
	}
	return d, nil
}

// projectFile returns .foreman.yml at HEAD, or nil if HEAD has none.
func projectFile(ctx context.Context, dir string) ([]byte, error) {
	if _, err := Git(ctx, dir, "cat-file", "-e", "HEAD:"+project.File); err != nil {
		if isExit(err, 128) || isExit(err, 1) {
			return nil, nil
		}
		return nil, err
	}
	out, err := gitOut(ctx, dir, nil, nil, "cat-file", "blob", "HEAD:"+project.File)
	if err != nil {
		return nil, err
	}
	return []byte(out), nil
}

// PrepareDev prepares a workspace for development: a fresh clone of the
// default branch with a new branch checked out.
func (w *Workspace) PrepareDev(ctx context.Context, repo config.Repo, number int, branch string) (Dirs, error) {
	d, err := w.Prepare(ctx, repo, number)
	if err != nil {
		return Dirs{}, err
	}
	if _, err := Git(ctx, d.Workdir, "checkout", "-q", "-b", branch); err != nil {
		return Dirs{}, err
	}
	return d, nil
}

// PrepareBranch prepares a workspace on an existing branch: a fresh clone
// of the default branch, which .foreman.yml is read from, then the last
// depth commits of branch, checked out. need must be among them: it is
// the commit the work started from.
func (w *Workspace) PrepareBranch(ctx context.Context, repo config.Repo, number int, branch string, depth int, need string) (Dirs, error) {
	d, err := w.Prepare(ctx, repo, number)
	if err != nil {
		return Dirs{}, err
	}
	env, secrets, err := w.auth(ctx, repo)
	if err != nil {
		return Dirs{}, err
	}
	if _, err := gitOut(ctx, d.Workdir, env, secrets, "fetch", "-q", "--no-tags", "--depth", strconv.Itoa(max(depth, 1)),
		"origin", "+refs/heads/"+branch+":refs/heads/"+branch); err != nil {
		return Dirs{}, err
	}
	if err := scanForSecrets(filepath.Join(d.Workdir, ".git"), secrets); err != nil {
		_ = os.RemoveAll(d.Workdir) // the credential must not stay on disk
		return Dirs{}, err
	}
	if _, err := Git(ctx, d.Workdir, "checkout", "-q", branch); err != nil {
		return Dirs{}, err
	}
	if _, err := Git(ctx, d.Workdir, "cat-file", "-e", need+"^{commit}"); err != nil {
		return Dirs{}, fmt.Errorf("branch %s does not reach %s within %d commits", branch, need, depth)
	}
	return d, nil
}

// PrepareAt prepares a workspace at commit sha of the default branch,
// such as a merge commit: a fresh clone of the default branch, then sha
// fetched and checked out, detached. Base is sha; ProjectFile stays the
// one of the default branch's tip, ProjectSHA, so a configuration
// updated after sha applies.
func (w *Workspace) PrepareAt(ctx context.Context, repo config.Repo, number int, sha string) (Dirs, error) {
	d, err := w.Prepare(ctx, repo, number)
	if err != nil {
		return Dirs{}, err
	}
	if d.Base != sha {
		env, secrets, err := w.auth(ctx, repo)
		if err != nil {
			return Dirs{}, err
		}
		if _, err := gitOut(ctx, d.Workdir, env, secrets, "fetch", "-q", "--no-tags", "--depth", "1", "origin", sha); err != nil {
			return Dirs{}, err
		}
		if err := scanForSecrets(filepath.Join(d.Workdir, ".git"), secrets); err != nil {
			_ = os.RemoveAll(d.Workdir) // the credential must not stay on disk
			return Dirs{}, err
		}
		if _, err := Git(ctx, d.Workdir, "checkout", "-q", "--detach", sha); err != nil {
			return Dirs{}, err
		}
		if d.Base, err = Git(ctx, d.Workdir, "rev-parse", "HEAD"); err != nil {
			return Dirs{}, err
		}
	}
	return d, nil
}

// Snapshot clones branch from an agent's workdir into
// data_dir/check/<owner>/<repo>/<issue>, a directory foreman owns, and
// returns it with the branch tip. Only committed history crosses over,
// through the git protocol: the agent's .git configuration, hooks and
// alternates and its uncommitted files stay behind.
func (w *Workspace) Snapshot(ctx context.Context, repo config.Repo, number int, workdir, branch string) (dir, tip string, err error) {
	dir = filepath.Join(w.DataDir, "check", repo.Owner, repo.Name, strconv.Itoa(number))
	if err := os.RemoveAll(dir); err != nil {
		return "", "", err
	}
	if err := os.MkdirAll(filepath.Dir(dir), 0o700); err != nil {
		return "", "", err
	}
	if _, err := Git(ctx, "", "clone", "-q", "--no-local", "--no-tags", "--single-branch", "--branch", branch, "--", workdir, dir); err != nil {
		_ = os.RemoveAll(dir)
		return "", "", err
	}
	tip, err = Git(ctx, dir, "rev-parse", "HEAD")
	return dir, tip, err
}

// Push pushes commit sha of the repository in dir to branch on GitHub.
// The token reaches the git process through its environment only, and
// the URL is given on the command line: no remote is stored. force
// replaces whatever the branch holds; otherwise only a fast-forward is
// accepted.
func (w *Workspace) Push(ctx context.Context, repo config.Repo, dir, sha, branch string, force bool) error {
	env, secrets, err := w.auth(ctx, repo)
	if err != nil {
		return err
	}
	ref := sha + ":refs/heads/" + branch
	if force {
		ref = "+" + ref
	}
	if _, err := gitOut(ctx, dir, env, secrets, "push", "-q", "--no-verify", "--", w.cloneURL(repo), ref); err != nil {
		return err
	}
	return scanForSecrets(filepath.Join(dir, ".git"), secrets)
}

func (w *Workspace) cloneURL(repo config.Repo) string {
	if w.CloneURL != nil {
		return w.CloneURL(repo)
	}
	return "https://github.com/" + repo.Owner + "/" + repo.Name + ".git"
}

// auth returns the environment that gives a git process the installation
// token as an HTTP header, and the strings to redact and scan for.
func (w *Workspace) auth(ctx context.Context, repo config.Repo) (env, secrets []string, err error) {
	tok, err := w.Tokens.Token(ctx, repo)
	if err != nil {
		return nil, nil, err
	}
	basic := base64.StdEncoding.EncodeToString([]byte("x-access-token:" + tok.Reveal()))
	// The header lives only in the git process's environment.
	return []string{
		"GIT_CONFIG_COUNT=2",
		"GIT_CONFIG_KEY_0=http.extraHeader",
		"GIT_CONFIG_VALUE_0=Authorization: Basic " + basic,
		"GIT_CONFIG_KEY_1=credential.helper",
		"GIT_CONFIG_VALUE_1=",
	}, []string{tok.Reveal(), basic}, nil
}

// clone makes a shallow clone of the default branch in dir and returns
// the branch name.
func (w *Workspace) clone(ctx context.Context, repo config.Repo, dir string) (string, error) {
	if err := os.RemoveAll(dir); err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(dir), 0o700); err != nil {
		return "", err
	}
	branch, err := w.Tokens.DefaultBranch(ctx, repo)
	if err != nil {
		return "", err
	}
	auth, secrets, err := w.auth(ctx, repo)
	if err != nil {
		return "", err
	}
	url := w.cloneURL(repo)
	if err := git(ctx, "", auth, secrets, "clone", "--depth", "1", "--single-branch", "--no-tags",
		"--branch", branch, "--", url, dir); err != nil {
		return "", err
	}
	// Belt and braces: none of this should be needed.
	if err := git(ctx, dir, nil, secrets, "remote", "set-url", "origin", url); err != nil {
		return "", err
	}
	if err := git(ctx, dir, nil, secrets, "config", "--local", "--unset-all", "http.extraHeader"); err != nil && !isExit(err, 5) {
		return "", err // 5: the key was not set
	}
	return branch, scanForSecrets(filepath.Join(dir, ".git"), secrets)
}

// Git runs git in dir with a clean configuration and returns its standard
// output, trimmed.
func Git(ctx context.Context, dir string, args ...string) (string, error) {
	out, err := gitOut(ctx, dir, nil, nil, args...)
	return strings.TrimSpace(out), err
}

func git(ctx context.Context, dir string, extraEnv, secrets []string, args ...string) error {
	_, err := gitOut(ctx, dir, extraEnv, secrets, args...)
	return err
}

// gitOut runs git with a clean configuration: no system or global config,
// no prompt, no hooks. extraEnv carries per-call settings; secrets are
// redacted from its error output.
func gitOut(ctx context.Context, dir string, extraEnv, secrets []string, args ...string) (string, error) {
	sub := args[0]
	// No background gc or maintenance: it would write in .git while
	// scanForSecrets reads it.
	args = append([]string{"-c", "core.hooksPath=" + os.DevNull, "-c", "gc.auto=0", "-c", "maintenance.auto=false"}, args...)
	cmd := exec.CommandContext(ctx, "git", args...) //nolint:gosec // fixed binary; args are built here, the URL from validated config
	cmd.Dir = dir
	cmd.Env = append([]string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + os.Getenv("HOME"),
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL=" + os.DevNull,
		"GIT_TERMINAL_PROMPT=0",
		"GIT_ASKPASS=",
		"SSH_ASKPASS=",
		"LC_ALL=C",
	}, extraEnv...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		for _, s := range secrets {
			msg = strings.ReplaceAll(msg, s, "[redacted]")
		}
		return "", &gitError{args: sub, msg: msg, err: err}
	}
	return stdout.String(), nil
}

type gitError struct {
	args string
	msg  string
	err  error
}

func (e *gitError) Error() string { return fmt.Sprintf("git %s: %v: %s", e.args, e.err, e.msg) }
func (e *gitError) Unwrap() error { return e.err }

func isExit(err error, code int) bool {
	var ee *exec.ExitError
	return errors.As(err, &ee) && ee.ExitCode() == code
}

// ErrTokenOnDisk means a credential was found in the clone.
var ErrTokenOnDisk = errors.New("credential found in the clone")

// scanForSecrets fails if any file under dir contains one of secrets.
func scanForSecrets(dir string, secrets []string) error {
	return filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || !d.Type().IsRegular() {
			return err
		}
		b, err := os.ReadFile(p) //nolint:gosec // p comes from walking our own clone
		if err != nil {
			return err
		}
		for _, s := range secrets {
			if bytes.Contains(b, []byte(s)) {
				rel, _ := filepath.Rel(dir, p) // p is under dir
				return fmt.Errorf("%w: .git/%s", ErrTokenOnDisk, rel)
			}
		}
		return nil
	})
}

// skills installs and verifies the skills pack in dir, and makes dir a
// Claude Code plugin whose skills are the installed ones.
func (w *Workspace) skills(dir string) error {
	if err := os.RemoveAll(dir); err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	if _, err := skillpack.Install(w.Skills, dir, agentName, w.Version); err != nil {
		return fmt.Errorf("install skills: %w", err)
	}
	r, err := skillpack.Verify(w.Skills, dir, w.Version)
	if err != nil {
		return fmt.Errorf("verify skills: %w", err)
	}
	if !r.OK() {
		return fmt.Errorf("verify skills: missing %v, modified %v, extra %v", r.Missing, r.Modified, r.Extra)
	}
	manifest, err := json.Marshal(map[string]string{
		"name":    "foreman",
		"version": w.Version,
		"skills":  "./.claude/skills",
	})
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Join(dir, ".claude-plugin"), 0o700); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, ".claude-plugin", "plugin.json"), manifest, 0o600)
}
