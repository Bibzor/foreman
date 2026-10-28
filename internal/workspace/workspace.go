// Package workspace prepares what an agent works on: a fresh shallow
// clone with no credential in it, and a verified copy of the skills
// outside the clone.
//
// The installation token reaches the git child process only, as an HTTP
// header passed through GIT_CONFIG_* environment variables: never in the
// command line, the remote URL or .git/config. After the clone, .git is
// scanned for the token and the workspace is discarded if it is found.
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
	if err := w.clone(ctx, repo, d.Workdir); err != nil {
		_ = os.RemoveAll(d.Workdir) // never leave a half-made clone
		return Dirs{}, err
	}
	if err := w.skills(d.SkillsDir); err != nil {
		return Dirs{}, err
	}
	return d, nil
}

func (w *Workspace) cloneURL(repo config.Repo) string {
	if w.CloneURL != nil {
		return w.CloneURL(repo)
	}
	return "https://github.com/" + repo.Owner + "/" + repo.Name + ".git"
}

func (w *Workspace) clone(ctx context.Context, repo config.Repo, dir string) error {
	if err := os.RemoveAll(dir); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(dir), 0o700); err != nil {
		return err
	}
	branch, err := w.Tokens.DefaultBranch(ctx, repo)
	if err != nil {
		return err
	}
	tok, err := w.Tokens.Token(ctx, repo)
	if err != nil {
		return err
	}
	basic := base64.StdEncoding.EncodeToString([]byte("x-access-token:" + tok.Reveal()))
	secrets := []string{tok.Reveal(), basic}
	url := w.cloneURL(repo)

	// The header lives only in this process's environment.
	auth := []string{
		"GIT_CONFIG_COUNT=2",
		"GIT_CONFIG_KEY_0=http.extraHeader",
		"GIT_CONFIG_VALUE_0=Authorization: Basic " + basic,
		"GIT_CONFIG_KEY_1=credential.helper",
		"GIT_CONFIG_VALUE_1=",
	}
	if err := git(ctx, "", auth, secrets, "clone", "--depth", "1", "--single-branch", "--no-tags",
		"--branch", branch, "--", url, dir); err != nil {
		return err
	}
	// Belt and braces: none of this should be needed.
	if err := git(ctx, dir, nil, secrets, "remote", "set-url", "origin", url); err != nil {
		return err
	}
	if err := git(ctx, dir, nil, secrets, "config", "--local", "--unset-all", "http.extraHeader"); err != nil && !isExit(err, 5) {
		return err // 5: the key was not set
	}
	return scanForSecrets(filepath.Join(dir, ".git"), secrets)
}

// git runs git with a clean configuration: no system or global config, no
// prompt. extraEnv carries per-call settings; secrets are redacted from
// its error output.
func git(ctx context.Context, dir string, extraEnv, secrets []string, args ...string) error {
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
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		for _, s := range secrets {
			msg = strings.ReplaceAll(msg, s, "[redacted]")
		}
		return &gitError{args: args[0], msg: msg, err: err}
	}
	return nil
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
