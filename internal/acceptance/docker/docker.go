// Package docker is the Docker acceptance backend: one container per
// repository, built from the merged commit, published on 127.0.0.1 only
// and hardened (read-only root, no capabilities, no new privileges,
// memory and process limits).
//
// docker is always run as argv, never through a shell, with arguments
// foreman builds itself from the validated .foreman.yml.
package docker

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/Bibzor/foreman/internal/acceptance"
	"github.com/Bibzor/foreman/internal/config"
	"github.com/Bibzor/foreman/internal/project"
)

// Limits of an acceptance container and of each docker step.
const (
	Memory       = "512m"
	PidsLimit    = 256
	BuildTimeout = 20 * time.Minute
	StepTimeout  = 2 * time.Minute
	SmokeTimeout = 60 * time.Second
)

// Labels put on images and containers, to find them again.
const (
	LabelRepo  = "foreman.repo"
	LabelIssue = "foreman.issue"
	LabelSHA   = "foreman.sha"
)

// Runner runs docker with args and returns its combined output.
type Runner interface {
	Run(ctx context.Context, args ...string) (string, error)
}

// Exec runs the docker binary from PATH, with an environment reduced to
// what the docker client needs.
type Exec struct{}

// Run runs docker args, as argv.
func (Exec) Run(ctx context.Context, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "docker", args...) //nolint:gosec // fixed binary; args are built by this package from validated config
	for _, k := range []string{"PATH", "HOME", "DOCKER_HOST", "DOCKER_CONTEXT", "DOCKER_CONFIG", "DOCKER_CERT_PATH", "DOCKER_TLS_VERIFY"} {
		if v, ok := os.LookupEnv(k); ok {
			cmd.Env = append(cmd.Env, k+"="+v)
		}
	}
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	err := cmd.Run()
	return out.String(), err
}

// Probe returns the HTTP status of a GET of url.
type Probe func(ctx context.Context, url string) (int, error)

// HTTPProbe is the real Probe: one GET, redirects not followed.
func HTTPProbe(ctx context.Context, url string) (int, error) {
	client := &http.Client{
		Timeout:       5 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return 0, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0, err
	}
	_ = resp.Body.Close() // only the status matters
	return resp.StatusCode, nil
}

// Deployer is the Docker acceptance.Deployer.
type Deployer struct {
	Runner Runner
	Probe  Probe
	// SmokeTimeout and Interval default to 60s and 1s.
	SmokeTimeout time.Duration
	Interval     time.Duration
}

var _ acceptance.Deployer = (*Deployer)(nil)

var notName = regexp.MustCompile(`[^a-z0-9]+`)

func slug(s string) string {
	return strings.Trim(notName.ReplaceAllString(strings.ToLower(s), "-"), "-")
}

// Names returns the image tag and the container name of a deployment:
// foreman-acc/<owner>-<repo>:<short sha> and foreman-acc-<owner>-<repo>.
func Names(repo config.Repo, sha string) (image, container string) {
	base := slug(repo.Owner) + "-" + slug(repo.Name)
	short := sha
	if len(short) > 12 {
		short = short[:12]
	}
	return "foreman-acc/" + base + ":" + slug(short), "foreman-acc-" + base
}

func labels(s acceptance.Spec) []string {
	return []string{
		"--label", LabelRepo + "=" + s.Repo.String(),
		"--label", LabelIssue + "=" + strconv.Itoa(s.Issue),
		"--label", LabelSHA + "=" + s.SHA,
	}
}

// pairs renders m as flag K=V pairs, sorted by name.
func pairs(flag string, m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var out []string
	for _, k := range keys {
		out = append(out, flag, k+"="+m[k])
	}
	return out
}

// BuildArgv is the docker build of a deployment.
func BuildArgv(s acceptance.Spec) []string {
	image, _ := Names(s.Repo, s.SHA)
	args := append([]string{"build"}, labels(s)...)
	args = append(args, pairs("--build-arg", s.Config.BuildArgs)...)
	return append(args, "-t", image, "-f", filepath.Join(s.Dir, filepath.FromSlash(s.Config.Dockerfile)), s.Dir)
}

// RunArgv is the docker run of a deployment: detached, published on
// 127.0.0.1 only, read-only with a tmpfs /tmp, no capabilities, no new
// privileges, memory and process limits.
func RunArgv(s acceptance.Spec) []string {
	image, name := Names(s.Repo, s.SHA)
	args := append([]string{"run", "-d", "--name", name}, labels(s)...)
	args = append(args, "-p", fmt.Sprintf("127.0.0.1:%d:%d", s.Config.HostPort, s.Config.ContainerPort))
	args = append(args, pairs("-e", s.Config.Env)...)
	return append(args,
		"--read-only", "--tmpfs", "/tmp",
		"--cap-drop", "ALL", "--security-opt", "no-new-privileges",
		"--memory", Memory, "--pids-limit", strconv.Itoa(PidsLimit),
		image)
}

func (d *Deployer) run(ctx context.Context, timeout time.Duration, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	return d.Runner.Run(ctx, args...)
}

func failed(stage, out string, err error) error {
	return &acceptance.StageError{Stage: stage, Log: acceptance.Tail(acceptance.Redact(strings.TrimSpace(out + "\n" + err.Error())))}
}

// Deploy builds the image, removes the repository's previous acceptance
// container and starts the new one.
func (d *Deployer) Deploy(ctx context.Context, s acceptance.Spec) (acceptance.Deployment, error) {
	if out, err := d.run(ctx, StepTimeout, "version", "--format", "{{.Server.Version}}"); err != nil {
		if ctx.Err() != nil {
			return acceptance.Deployment{}, ctx.Err()
		}
		return acceptance.Deployment{}, fmt.Errorf("%w: docker version: %w: %s", acceptance.ErrUnavailable, err,
			acceptance.Tail(acceptance.Redact(strings.TrimSpace(out))))
	}
	if err := dockerfileInside(s.Dir, s.Config.Dockerfile); err != nil {
		return acceptance.Deployment{}, &acceptance.StageError{Stage: "build", Log: err.Error()}
	}
	if out, err := d.run(ctx, BuildTimeout, BuildArgv(s)...); err != nil {
		if ctx.Err() != nil {
			return acceptance.Deployment{}, ctx.Err()
		}
		return acceptance.Deployment{}, failed("build", out, err)
	}
	out, err := d.run(ctx, StepTimeout, "ps", "-aq", "--filter", "label="+LabelRepo+"="+s.Repo.String())
	if err != nil {
		return acceptance.Deployment{}, failed("run", out, err)
	}
	for _, id := range strings.Fields(out) {
		if out, err := d.run(ctx, StepTimeout, "rm", "-f", id); err != nil {
			return acceptance.Deployment{}, failed("run", out, err)
		}
	}
	_, name := Names(s.Repo, s.SHA)
	dep := acceptance.Deployment{ID: name, URL: fmt.Sprintf("http://localhost:%d", s.Config.HostPort), SHA: s.SHA, Spec: s}
	if out, err := d.run(ctx, StepTimeout, RunArgv(s)...); err != nil {
		if ctx.Err() != nil {
			return acceptance.Deployment{}, ctx.Err()
		}
		_ = d.Stop(ctx, dep) // a failed run may leave a created container
		return acceptance.Deployment{}, failed("run", out, err)
	}
	return dep, nil
}

// dockerfileInside refuses a Dockerfile that is not a regular file inside
// dir once symlinks are resolved: the docker client would read it from
// anywhere.
func dockerfileInside(dir, rel string) error {
	root, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return err
	}
	p, err := filepath.EvalSymlinks(filepath.Join(dir, filepath.FromSlash(rel)))
	if err != nil {
		return fmt.Errorf("%s: not found in the merged commit", rel)
	}
	if r, err := filepath.Rel(root, p); err != nil || r == ".." || strings.HasPrefix(r, ".."+string(filepath.Separator)) {
		return fmt.Errorf("%s: resolves outside the repository", rel)
	}
	if fi, err := os.Stat(p); err != nil || !fi.Mode().IsRegular() {
		return fmt.Errorf("%s: not a regular file", rel)
	}
	return nil
}

// Smoke polls each check on 127.0.0.1:<host_port> until it answers its
// status. All checks share one deadline. On failure the error carries the
// container's last logs.
func (d *Deployer) Smoke(ctx context.Context, dep acceptance.Deployment, checks []project.Smoke) error {
	timeout, interval := d.SmokeTimeout, d.Interval
	if timeout <= 0 {
		timeout = SmokeTimeout
	}
	if interval <= 0 {
		interval = time.Second
	}
	sctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	for _, c := range checks {
		url := fmt.Sprintf("http://127.0.0.1:%d%s", dep.Spec.Config.HostPort, c.Path)
		var last string
		for {
			status, err := d.Probe(sctx, url)
			if err == nil && status == c.Status {
				break
			}
			if err != nil {
				last = err.Error()
			} else {
				last = fmt.Sprintf("status %d", status)
			}
			select {
			case <-sctx.Done():
				if ctx.Err() != nil {
					return ctx.Err()
				}
				logs, _ := d.run(ctx, StepTimeout, "logs", "--tail", "100", dep.ID) // best effort: the timeout is the error
				return &acceptance.StageError{Stage: "smoke", Log: acceptance.Tail(acceptance.Redact(fmt.Sprintf(
					"GET %s: want %d, got %s after %s\n\ncontainer logs:\n%s", c.Path, c.Status, last, timeout, logs)))}
			case <-time.After(interval):
			}
		}
	}
	return nil
}

// Stop removes the deployment's container.
func (d *Deployer) Stop(ctx context.Context, dep acceptance.Deployment) error {
	out, err := d.run(ctx, StepTimeout, "rm", "-f", dep.ID)
	if err != nil {
		return errors.Join(err, errors.New(acceptance.Redact(strings.TrimSpace(out))))
	}
	return nil
}
