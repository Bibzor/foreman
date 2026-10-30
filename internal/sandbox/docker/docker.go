// Package docker is the Docker sandbox: one throwaway container per
// command, with the workspace bind-mounted at /work.
//
// Every container has a read-only root with a tmpfs /tmp, no capability,
// no new privileges, the user's uid instead of root, memory, CPU and
// process limits, and no environment from the host: only HOME=/tmp and the
// project's sandbox env. The project's commands run with no network, the
// caches and the workspace's .git read-only; only setup has the network,
// and it never sees the agent's workspace.
//
// docker is always run as argv, never through a shell, with arguments
// built from a validated sandbox.Spec.
package docker

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/Bibzor/foreman/internal/config"
	"github.com/Bibzor/foreman/internal/sandbox"
)

// Labels put on sandbox containers, to find them again. Acceptance
// containers carry foreman.repo and foreman.issue too, never
// foreman.sandbox.
const (
	LabelSandbox = "foreman.sandbox" // setup or exec
	LabelRepo    = "foreman.repo"
	LabelIssue   = "foreman.issue"
)

// StepTimeout bounds docker calls that run no project code.
const StepTimeout = 2 * time.Minute

// Runner runs docker with args and env, its output written to out.
type Runner interface {
	Run(ctx context.Context, env []string, out io.Writer, args ...string) error
}

// Exec runs the docker binary from PATH, with PATH, HOME and env only.
type Exec struct{}

// Run runs docker args, as argv.
func (Exec) Run(ctx context.Context, env []string, out io.Writer, args ...string) error {
	cmd := exec.CommandContext(ctx, "docker", args...) //nolint:gosec // fixed binary; args are built by this package from a validated spec
	for _, k := range []string{"PATH", "HOME"} {
		if v, ok := os.LookupEnv(k); ok {
			cmd.Env = append(cmd.Env, k+"="+v)
		}
	}
	cmd.Env = append(cmd.Env, env...)
	cmd.Stdout, cmd.Stderr = out, out
	cmd.WaitDelay = 5 * time.Second
	return cmd.Run()
}

// Sandbox is the Docker sandbox.Sandbox.
type Sandbox struct{ Runner Runner }

var _ sandbox.Sandbox = (*Sandbox)(nil)

var notName = regexp.MustCompile(`[^a-z0-9]+`)

// containerName is foreman-sbx-<owner>-<repo>-<issue>-<random>: two runs
// never share a name.
func containerName(s sandbox.Spec) string {
	b := make([]byte, 4)
	_, _ = rand.Read(b) // never fails
	slug := strings.Trim(notName.ReplaceAllString(strings.ToLower(s.Repo), "-"), "-")
	return "foreman-sbx-" + slug + "-" + strconv.Itoa(s.Issue) + "-" + hex.EncodeToString(b)
}

// common is the part of the docker run shared by setup and exec.
func common(s sandbox.Spec, name, role string) []string {
	l := s.Limits
	args := []string{"run", "--rm", "--name", name,
		"--label", LabelSandbox + "=" + role,
		"--label", LabelRepo + "=" + s.Repo,
		"--label", LabelIssue + "=" + strconv.Itoa(s.Issue),
		"--read-only", "--tmpfs", "/tmp:rw,exec,nosuid,nodev,size=" + l.Tmpfs,
		"--cap-drop", "ALL", "--security-opt", "no-new-privileges",
		"--user", s.User,
		"--memory", l.Memory, "--memory-swap", l.Memory, "--cpus", l.CPUs, "--pids-limit", strconv.Itoa(l.Pids),
		"--workdir", "/work",
		"--env", "HOME=/tmp",
	}
	keys := make([]string, 0, len(s.Env))
	for k := range s.Env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		// Always K=V: a bare K would copy the host's value.
		args = append(args, "--env", k+"="+s.Env[k])
	}
	return args
}

func bind(src, dst string, readonly bool) []string {
	m := "type=bind,src=" + src + ",dst=" + dst
	if readonly {
		m += ",readonly"
	}
	return []string{"--mount", m}
}

// command ends a docker run: the program as the entrypoint, so the
// image's own entrypoint never wraps it, then its arguments.
func command(args []string, image string, argv []string) []string {
	args = append(args, "--entrypoint", argv[0], image)
	return append(args, argv[1:]...)
}

// SetupArgv is the docker run of a setup command: network access, the
// setup tree at /work, the caches writable.
func SetupArgv(s sandbox.Spec, c sandbox.Command, name string) []string {
	args := common(s, name, "setup")
	args = append(args, "--network", "bridge")
	args = append(args, bind(s.Workspace, "/work", false)...)
	for _, m := range s.Caches {
		args = append(args, bind(m.Host, m.Container, false)...)
	}
	return command(args, s.Image, c.Argv)
}

// ExecArgv is the docker run of a project command: no network, the
// workspace at /work with its .git read-only, the caches read-only.
func ExecArgv(s sandbox.Spec, c sandbox.Command, name string) []string {
	args := common(s, name, "exec")
	args = append(args, "--network", "none")
	args = append(args, bind(s.Workspace, "/work", false)...)
	args = append(args, bind(filepath.Join(s.Workspace, ".git"), "/work/.git", true)...)
	for _, m := range s.Caches {
		args = append(args, bind(m.Host, m.Container, true)...)
	}
	return command(args, s.Image, c.Argv)
}

// Setup runs every command of s, in order.
func (d *Sandbox) Setup(ctx context.Context, s sandbox.Spec, out io.Writer) error {
	if err := s.Validate(); err != nil {
		return err
	}
	for _, c := range s.Commands {
		if err := d.run(ctx, s, SetupArgv(s, c, containerName(s)), out); err != nil {
			return fmt.Errorf("setup `%s`: %w", strings.Join(c.Argv, " "), err)
		}
	}
	return nil
}

// Exec runs the command name of s.
func (d *Sandbox) Exec(ctx context.Context, s sandbox.Spec, name string, out io.Writer) error {
	if err := s.Validate(); err != nil {
		return err
	}
	c, ok := s.Command(name)
	if !ok {
		return fmt.Errorf("no sandbox command %q", name)
	}
	// The read-only .git mount must cover the real .git: refuse a
	// symlink or a .git file pointing elsewhere.
	if fi, err := os.Lstat(filepath.Join(s.Workspace, ".git")); err != nil || !fi.IsDir() {
		return fmt.Errorf("%s/.git is not a directory", s.Workspace)
	}
	return d.run(ctx, s, ExecArgv(s, c, containerName(s)), out)
}

// run runs one container. On timeout or cancellation the container is
// removed: killing the client does not stop it.
func (d *Sandbox) run(ctx context.Context, s sandbox.Spec, args []string, out io.Writer) error {
	name := args[3] // run --rm --name <name>
	rctx, cancel := context.WithTimeout(ctx, s.Timeout())
	defer cancel()
	err := d.Runner.Run(rctx, s.DockerEnv, out, args...)
	if rctx.Err() != nil {
		rm, cancel := context.WithTimeout(context.WithoutCancel(ctx), StepTimeout)
		defer cancel()
		_ = d.Runner.Run(rm, s.DockerEnv, io.Discard, "rm", "-f", name) // best effort: the timeout is the error
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("%w after %s", sandbox.ErrTimeout, s.Timeout())
	}
	var ee interface{ ExitCode() int }
	if errors.As(err, &ee) {
		// 125: docker itself failed (daemon, image, mount), not the
		// command.
		if ee.ExitCode() == 125 {
			return fmt.Errorf("docker run: %w", err)
		}
		return &sandbox.ExitError{Code: ee.ExitCode()}
	}
	return err
}

// Cleanup removes the issue's sandbox containers that are left.
func (d *Sandbox) Cleanup(ctx context.Context, repo config.Repo, issue int) error {
	ctx, cancel := context.WithTimeout(ctx, StepTimeout)
	defer cancel()
	env := hostDockerEnv()
	var ids strings.Builder
	if err := d.Runner.Run(ctx, env, &ids, "ps", "-aq",
		"--filter", "label="+LabelSandbox,
		"--filter", "label="+LabelRepo+"="+repo.String(),
		"--filter", "label="+LabelIssue+"="+strconv.Itoa(issue)); err != nil {
		return fmt.Errorf("docker ps: %w: %s", err, strings.TrimSpace(ids.String()))
	}
	for _, id := range strings.Fields(ids.String()) {
		var out strings.Builder
		if err := d.Runner.Run(ctx, env, &out, "rm", "-f", id); err != nil {
			return fmt.Errorf("docker rm: %w: %s", err, strings.TrimSpace(out.String()))
		}
	}
	return nil
}

func hostDockerEnv() []string {
	var env []string
	for _, k := range sandbox.DockerEnvNames {
		if v, ok := os.LookupEnv(k); ok {
			env = append(env, k+"="+v)
		}
	}
	return env
}
