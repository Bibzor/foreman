// Package sandbox runs the project's commands in a container, never on
// the host: the agent's tests and lint, foreman's own check, and the
// setup that fills the caches.
//
// A Spec says what to run and where. foreman writes the agent's spec in
// the plugin directory, outside the clone, where the agent's file tools
// cannot reach; `foreman sandbox exec` reads it back and refuses one that
// is not under data_dir/skills, not the user's own or not mode 0600.
//
// The implementation, Docker, lives in sandbox/docker.
package sandbox

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/Bibzor/foreman/internal/config"
	"github.com/Bibzor/foreman/internal/project"
)

// Sandbox runs commands in a container.
type Sandbox interface {
	// Setup runs every command of s in order, with network access, the
	// caches writable.
	Setup(ctx context.Context, s Spec, out io.Writer) error
	// Exec runs the command name of s with no network, the caches and
	// the workspace's .git read-only. A command that ran and failed is an
	// *ExitError.
	Exec(ctx context.Context, s Spec, name string, out io.Writer) error
	// Cleanup removes the issue's sandbox containers that are left, such
	// as one whose client was killed.
	Cleanup(ctx context.Context, repo config.Repo, issue int) error
}

// ExitError is a command that ran in the sandbox and exited non-zero.
type ExitError struct{ Code int }

func (e *ExitError) Error() string { return "exit status " + strconv.Itoa(e.Code) }

// ErrTimeout is a command that ran out of time; its container is removed.
var ErrTimeout = errors.New("timed out")

// Names of the files and variables shared with `foreman sandbox exec`.
const (
	SpecFile = "sandbox.json"
	EnvSpec  = "FOREMAN_SANDBOX_SPEC"
)

// Timeouts of each command.
const (
	SetupTimeout = 20 * time.Minute
	// AgentTimeout is Claude Code's longest Bash timeout: a longer command
	// would be killed under the agent anyway.
	AgentTimeout = 10 * time.Minute
	CheckTimeout = 15 * time.Minute
)

// Limits are the container's resources.
type Limits struct {
	Memory string `json:"memory"` // also the memory+swap limit: no swap
	CPUs   string `json:"cpus"`
	Pids   int    `json:"pids"`
	Tmpfs  string `json:"tmpfs"` // size of /tmp, counted in Memory
}

// DefaultLimits leave room for a Go or Node build and its tests.
var DefaultLimits = Limits{Memory: "4g", CPUs: "2", Pids: 1024, Tmpfs: "2g"}

// Mount is a cache: a host directory foreman owns, at a container path.
type Mount struct {
	Host      string `json:"host"`
	Container string `json:"container"`
}

// Command is a project command, by the name the agent runs it with.
type Command struct {
	Name string   `json:"name"`
	Argv []string `json:"argv"`
}

// Spec is one sandbox: the image, the workspace mounted at /work, the
// caches, the environment, the commands it may run, and its limits.
type Spec struct {
	Version   int               `json:"version"`
	Repo      string            `json:"repo"` // owner/name, for the labels
	Issue     int               `json:"issue"`
	Workspace string            `json:"workspace"`
	Image     string            `json:"image"`
	Caches    []Mount           `json:"caches"`
	Env       map[string]string `json:"env"`
	Commands  []Command         `json:"commands"`
	User      string            `json:"user"` // uid:gid, never root
	Limits    Limits            `json:"limits"`
	TimeoutS  int               `json:"timeout_s"`
	// DockerEnv are the docker client's settings, such as DOCKER_HOST,
	// for `foreman sandbox exec`, whose environment is the agent's.
	DockerEnv []string `json:"docker_env"`
}

const specVersion = 1

// Timeout is the limit of each command.
func (s Spec) Timeout() time.Duration { return time.Duration(s.TimeoutS) * time.Second }

// Command returns the command name of s.
func (s Spec) Command(name string) (Command, bool) {
	for _, c := range s.Commands {
		if c.Name == name {
			return c, true
		}
	}
	return Command{}, false
}

// Names returns the commands of p by the name the agent runs them with:
// test, lint, then the index of each extra command.
func Names(p project.Config) []Command {
	out := []Command{{Name: "test", Argv: p.Test.Argv}, {Name: "lint", Argv: p.Lint.Argv}}
	for i, c := range p.Extra {
		out = append(out, Command{Name: strconv.Itoa(i), Argv: c.Argv})
	}
	return out
}

// Line is the command line the agent runs to execute command name: the
// foreman binary and the data directory are fixed in it, so the agent
// cannot point foreman at another spec root.
func Line(foreman, dataDir, name string) string {
	return foreman + " sandbox exec --data-dir " + dataDir + " " + name
}

// path is an absolute host path that can be put, unquoted, in a docker
// --mount option and in a Claude Code permission rule.
var hostPath = regexp.MustCompile(`^(/[A-Za-z0-9_.@+-]+)+$`)

// HostPath validates such a path.
func HostPath(p string) error {
	if !hostPath.MatchString(p) || filepath.Clean(p) != p {
		return fmt.Errorf("%q: foreman needs a clean absolute path made of letters, digits and _.@+-", p)
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == "." || seg == ".." {
			return fmt.Errorf("%q: no . or .. segment", p)
		}
	}
	return nil
}

// CacheDirs returns the host directories of the caches of an issue:
// data_dir/cache/<owner>/<repo>/<issue>/<i>.
func CacheDirs(dataDir string, repo config.Repo, issue int, paths []string) []Mount {
	var out []Mount
	for i, p := range paths {
		out = append(out, Mount{
			Host:      filepath.Join(dataDir, "cache", repo.Owner, repo.Name, strconv.Itoa(issue), strconv.Itoa(i)),
			Container: p,
		})
	}
	return out
}

// ResetCaches empties the caches of an issue and creates their
// directories. The Go module cache is read-only on disk: it is made
// writable first.
func ResetCaches(dataDir string, repo config.Repo, issue int, caches []Mount) error {
	root := filepath.Join(dataDir, "cache", repo.Owner, repo.Name, strconv.Itoa(issue))
	if err := RemoveAll(root); err != nil {
		return err
	}
	for _, c := range caches {
		if err := os.MkdirAll(c.Host, 0o700); err != nil {
			return err
		}
	}
	return nil
}

// RemoveAll removes dir like os.RemoveAll, after making every directory
// in it writable.
func RemoveAll(dir string) error {
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return err
		}
		if d.IsDir() {
			return os.Chmod(p, 0o700) //nolint:gosec // our own directory, made writable to be removed
		}
		return nil
	})
	if err != nil {
		return err
	}
	return os.RemoveAll(dir)
}

// DockerEnvNames are the docker client variables a spec carries.
var DockerEnvNames = []string{"DOCKER_HOST", "DOCKER_CONTEXT", "DOCKER_CONFIG", "DOCKER_CERT_PATH", "DOCKER_TLS_VERIFY"}

// New returns the spec of the commands of p on workspace, for an issue.
func New(p project.Config, dataDir string, repo config.Repo, issue int, workspace string, timeout time.Duration) Spec {
	s := Spec{
		Version:   specVersion,
		Repo:      repo.String(),
		Issue:     issue,
		Workspace: workspace,
		Image:     p.Sandbox.Image,
		Caches:    CacheDirs(dataDir, repo, issue, p.Sandbox.Cache),
		Env:       p.Sandbox.Env,
		Commands:  Names(p),
		User:      fmt.Sprintf("%d:%d", os.Getuid(), os.Getgid()),
		Limits:    DefaultLimits,
		TimeoutS:  int(timeout / time.Second),
	}
	for _, k := range DockerEnvNames {
		if v, ok := os.LookupEnv(k); ok {
			s.DockerEnv = append(s.DockerEnv, k+"="+v)
		}
	}
	return s
}

// Setup returns the spec of the setup commands of p, on tree.
func Setup(p project.Config, dataDir string, repo config.Repo, issue int, tree string) Spec {
	s := New(p, dataDir, repo, issue, tree, SetupTimeout)
	s.Commands = nil
	for i, c := range p.Sandbox.Setup {
		s.Commands = append(s.Commands, Command{Name: "setup-" + strconv.Itoa(i), Argv: c.Argv})
	}
	return s
}

var (
	user     = regexp.MustCompile(`^[1-9][0-9]*:[0-9]+$`)
	size     = regexp.MustCompile(`^[1-9][0-9]*[kmg]$`)
	cpus     = regexp.MustCompile(`^[0-9]+(\.[0-9]+)?$`)
	cmdName  = regexp.MustCompile(`^(test|lint|[0-9]+|setup-[0-9]+)$`)
	maxLimit = time.Hour
)

// Validate checks every field of s: the docker command line is built from
// it.
func (s Spec) Validate() error {
	var errs []error
	if s.Version != specVersion {
		errs = append(errs, fmt.Errorf("version %d, want %d", s.Version, specVersion))
	}
	if _, err := config.ParseRepo(s.Repo); err != nil || s.Issue <= 0 {
		errs = append(errs, fmt.Errorf("repo %q issue %d", s.Repo, s.Issue))
	}
	if err := HostPath(s.Workspace); err != nil {
		errs = append(errs, fmt.Errorf("workspace: %w", err))
	}
	if err := project.ImageRef(s.Image); err != nil {
		errs = append(errs, fmt.Errorf("image: %w", err))
	}
	for _, c := range s.Caches {
		if err := HostPath(c.Host); err != nil {
			errs = append(errs, fmt.Errorf("cache: %w", err))
		}
		if err := project.CachePath(c.Container); err != nil {
			errs = append(errs, fmt.Errorf("cache: %w", err))
		}
	}
	if err := project.ValidEnv("env", s.Env); err != nil {
		errs = append(errs, err)
	}
	if len(s.Commands) == 0 {
		errs = append(errs, errors.New("no command"))
	}
	seen := map[string]bool{}
	for _, c := range s.Commands {
		if !cmdName.MatchString(c.Name) || seen[c.Name] {
			errs = append(errs, fmt.Errorf("command name %q", c.Name))
		}
		seen[c.Name] = true
		if err := project.ValidArgv(c.Argv); err != nil {
			errs = append(errs, fmt.Errorf("command %s: %w", c.Name, err))
		}
	}
	if !user.MatchString(s.User) {
		errs = append(errs, fmt.Errorf("user %q: must be uid:gid, not root", s.User))
	}
	l := s.Limits
	if !size.MatchString(l.Memory) || !size.MatchString(l.Tmpfs) || !cpus.MatchString(l.CPUs) || l.Pids <= 0 {
		errs = append(errs, fmt.Errorf("limits %+v", l))
	}
	if s.Timeout() <= 0 || s.Timeout() > maxLimit {
		errs = append(errs, fmt.Errorf("timeout %s", s.Timeout()))
	}
	for _, kv := range s.DockerEnv {
		k, v, _ := strings.Cut(kv, "=")
		if !isDockerEnv(k) || strings.ContainsAny(v, "\n\r\x00") {
			errs = append(errs, fmt.Errorf("docker env %q", k))
		}
	}
	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("invalid sandbox spec: %w", err)
	}
	return nil
}

func isDockerEnv(k string) bool {
	for _, n := range DockerEnvNames {
		if k == n {
			return true
		}
	}
	return false
}

// Write writes s, validated, to dir/sandbox.json, mode 0600, and returns
// the path.
func Write(dir string, s Spec) (string, error) {
	if err := s.Validate(); err != nil {
		return "", err
	}
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return "", err
	}
	p := filepath.Join(dir, SpecFile)
	tmp, err := os.CreateTemp(dir, SpecFile+".*")
	if err != nil {
		return "", err
	}
	defer func() { _ = os.Remove(tmp.Name()) }() // gone after the rename
	if _, err := tmp.Write(b); err != nil {
		_ = tmp.Close() // the write error is the one to report
		return "", err
	}
	if err := tmp.Close(); err != nil {
		return "", err
	}
	if err := os.Chmod(tmp.Name(), 0o600); err != nil {
		return "", err
	}
	return p, os.Rename(tmp.Name(), p)
}

// getuid is os.Getuid, a variable for tests.
var getuid = os.Getuid

// Read reads and validates the spec at path for `foreman sandbox exec`.
// It refuses a spec that, once symlinks are resolved, is not under
// dataDir/skills, is not a regular file owned by the current user with
// mode 0600, or lies inside its own workspace. The workspace and the
// caches must be under dataDir too.
func Read(path, dataDir string) (Spec, error) {
	if path == "" {
		return Spec{}, errors.New(EnvSpec + " is not set")
	}
	if !filepath.IsAbs(path) || !filepath.IsAbs(dataDir) {
		return Spec{}, fmt.Errorf("the spec %q and the data directory %q must be absolute", path, dataDir)
	}
	root, err := filepath.EvalSymlinks(dataDir)
	if err != nil {
		return Spec{}, fmt.Errorf("data directory: %w", err)
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return Spec{}, fmt.Errorf("spec: %w", err)
	}
	if !within(resolved, filepath.Join(root, "skills")) {
		return Spec{}, fmt.Errorf("spec %s is not under %s", path, filepath.Join(dataDir, "skills"))
	}
	fi, err := os.Lstat(resolved)
	if err != nil {
		return Spec{}, err
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	switch {
	case !fi.Mode().IsRegular():
		return Spec{}, fmt.Errorf("spec %s is not a regular file", path)
	case fi.Mode().Perm() != 0o600:
		return Spec{}, fmt.Errorf("spec %s has mode %#o, want 0600", path, fi.Mode().Perm())
	case !ok || int(st.Uid) != getuid():
		return Spec{}, fmt.Errorf("spec %s is not owned by the current user", path)
	}
	b, err := os.ReadFile(resolved)
	if err != nil {
		return Spec{}, err
	}
	var s Spec
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&s); err != nil {
		return Spec{}, fmt.Errorf("spec %s: %w", path, err)
	}
	if err := s.Validate(); err != nil {
		return Spec{}, err
	}
	ws, err := filepath.EvalSymlinks(s.Workspace)
	if err != nil {
		return Spec{}, fmt.Errorf("workspace: %w", err)
	}
	if !within(ws, root) || within(resolved, ws) {
		return Spec{}, fmt.Errorf("workspace %s must be under %s and not hold the spec", s.Workspace, dataDir)
	}
	for _, c := range s.Caches {
		h, err := filepath.EvalSymlinks(c.Host)
		if err != nil || !within(h, filepath.Join(root, "cache")) {
			return Spec{}, fmt.Errorf("cache %s must be under %s", c.Host, filepath.Join(dataDir, "cache"))
		}
	}
	return s, nil
}

// within reports whether p is dir or under it; both are clean.
func within(p, dir string) bool {
	rel, err := filepath.Rel(dir, p)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}
