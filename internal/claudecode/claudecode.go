// Package claudecode runs Claude Code non-interactively. Run is shared by
// every step; Qualifier is the qualification agent.
//
// Every run uses --restricted, which confines the file tools to the
// working directory (symlinks pointing outside included) and ignores the
// repository's own .claude settings, hooks and MCP servers. The skills
// come from a verified plugin directory outside the clone. The
// environment is rebuilt from an allowlist, so GitHub credentials and
// anything else foreman inherits never reach the agent.
//
// This is not isolation: the agent still runs as the local user.
package claudecode

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/Bibzor/foreman/internal/qualify"
)

// Runner executes the claude binary. It is a variable for tests.
type Runner func(ctx context.Context, dir string, env, args []string, stdin string) (stdout, stderr []byte, err error)

// Claude runs Claude Code.
type Claude struct {
	Binary   string        // default "claude"
	MaxTurns int           // default 30
	Timeout  time.Duration // default 10 minutes
	Run      Runner        // default: os/exec
	Environ  func() []string
}

// Session is one non-interactive run.
type Session struct {
	Dir       string   // working directory, the only one the file tools can read
	PluginDir string   // verified skills
	Tools     []string // the only tools available
	Prompt    string
}

// ReadOnlyTools are the tools of a qualification: reading the clone and
// loading a skill. Nothing writes, runs commands or reaches the network.
var ReadOnlyTools = []string{"Read", "Grep", "Glob", "Skill"}

// Args returns the claude command line for s. The prompt is sent on
// stdin, so issue text never appears in the process list.
func (c *Claude) Args(s Session) []string {
	tools := strings.Join(s.Tools, ",")
	return []string{
		"-p",
		"--output-format", "json",
		"--tools", tools,
		"--allowedTools", tools,
		"--permission-mode", "dontAsk",
		"--restricted",
		"--strict-mcp-config",
		"--no-session-persistence",
		"--max-turns", strconv.Itoa(c.maxTurns()),
		"--plugin-dir", s.PluginDir,
	}
}

func (c *Claude) maxTurns() int {
	if c.MaxTurns > 0 {
		return c.MaxTurns
	}
	return 30
}

// envAllowed lists the variables passed to the agent: what a process
// needs to run and what Claude Code needs to authenticate. Nothing else.
func envAllowed(kv string) bool {
	name, _, _ := strings.Cut(kv, "=")
	switch name {
	case "PATH", "HOME", "USER", "LOGNAME", "LANG", "TMPDIR", "TERM", "SHELL":
		return true
	}
	return strings.HasPrefix(name, "LC_") || strings.HasPrefix(name, "ANTHROPIC_") || strings.HasPrefix(name, "CLAUDE_CODE_")
}

// Env filters environ down to the allowlist.
func Env(environ []string) []string {
	var out []string
	for _, kv := range environ {
		if envAllowed(kv) {
			out = append(out, kv)
		}
	}
	return out
}

// ErrAgent is a failed agent run: it crashed, timed out, hit the turn
// limit or reported an error. The message is for the local log only.
var ErrAgent = errors.New("agent run failed")

// Exec runs one session and returns the agent's final text.
func (c *Claude) Exec(ctx context.Context, s Session) (string, error) {
	timeout := c.Timeout
	if timeout <= 0 {
		timeout = 10 * time.Minute
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	environ := os.Environ
	if c.Environ != nil {
		environ = c.Environ
	}
	run := c.Run
	if run == nil {
		run = c.execRun
	}
	stdout, stderr, err := run(ctx, s.Dir, Env(environ()), c.Args(s), s.Prompt)
	if ctx.Err() != nil {
		return "", fmt.Errorf("%w: timed out after %s", ErrAgent, timeout)
	}
	if err != nil {
		return "", fmt.Errorf("%w: %w: %s", ErrAgent, err, tail(stderr))
	}
	var env struct {
		Type    string `json:"type"`
		Subtype string `json:"subtype"`
		IsError bool   `json:"is_error"`
		Result  string `json:"result"`
	}
	if err := json.Unmarshal(bytes.TrimSpace(stdout), &env); err != nil || env.Type != "result" {
		return "", &qualify.InvalidOutputError{Raw: string(stdout), Err: errors.New("not a Claude Code JSON result")}
	}
	if env.IsError || env.Subtype != "success" {
		return "", fmt.Errorf("%w: %s", ErrAgent, env.Subtype)
	}
	return env.Result, nil
}

func (c *Claude) execRun(ctx context.Context, dir string, env, args []string, stdin string) (stdout, stderr []byte, err error) {
	bin := c.Binary
	if bin == "" {
		bin = "claude"
	}
	cmd := exec.CommandContext(ctx, bin, args...) //nolint:gosec // binary from configuration, args built by Args
	cmd.Dir = dir
	cmd.Env = env
	cmd.Stdin = strings.NewReader(stdin)
	var out, errOut bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errOut
	cmd.WaitDelay = 5 * time.Second
	err = cmd.Run()
	return out.Bytes(), errOut.Bytes(), err
}

func tail(b []byte) string {
	s := strings.TrimSpace(string(b))
	if len(s) > 500 {
		s = "..." + s[len(s)-500:]
	}
	return s
}

// Qualifier is the Claude Code qualification agent.
type Qualifier struct{ Claude *Claude }

// Qualify runs the foreman-qualify skill on the issue snapshot, with
// read-only tools, and validates the output.
func (q Qualifier) Qualify(ctx context.Context, req qualify.Request) (qualify.Result, error) {
	text, err := q.Claude.Exec(ctx, Session{
		Dir:       req.Workdir,
		PluginDir: req.SkillsDir,
		Tools:     ReadOnlyTools,
		Prompt:    qualify.Prompt(req.Issue),
	})
	if err != nil {
		return qualify.Result{}, err
	}
	r, err := qualify.Parse(text)
	if err != nil {
		return qualify.Result{}, &qualify.InvalidOutputError{Raw: text, Err: err}
	}
	return r, nil
}
