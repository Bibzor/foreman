package claudecode

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/Bibzor/foreman/internal/develop"
	"github.com/Bibzor/foreman/internal/gitenv"
	"github.com/Bibzor/foreman/internal/project"
	"github.com/Bibzor/foreman/internal/qualify"
	"github.com/Bibzor/foreman/internal/sandbox"
)

// Limits of the development and pre-review runs.
const (
	DevMaxTurns       = 150
	DevTimeout        = 45 * time.Minute
	PreReviewMaxTurns = 40
	PreReviewTimeout  = 10 * time.Minute
)

// DevTools returns the tools of a development run and the rules that
// allow them. Files are read and written in the clone only, never its
// .git; Bash runs only the project's commands, in the sandbox through
// `foreman sandbox exec`, and the git commands that make commits, with no
// pager and no external diff. Nothing pushes, calls gh or reaches the
// network on purpose.
func DevTools(cmds []develop.AgentCommand) (tools, allowed []string) {
	tools = []string{"Read", "Grep", "Glob", "Edit", "Write", "Skill", "Bash"}
	allowed = []string{"Read", "Grep", "Glob", "Edit", "Write", "Skill"}
	for _, c := range cmds {
		allowed = append(allowed, "Bash("+c.Line+")")
	}
	for _, g := range develop.GitCommands {
		allowed = append(allowed, "Bash("+g+":*)")
	}
	return tools, allowed
}

// Developer is the Claude Code development and pre-review agent.
type Developer struct {
	Claude *Claude
	// GitName and GitEmail sign the agent's commits.
	GitName  string
	GitEmail string
	// Foreman is the absolute path of the foreman binary and DataDir its
	// data directory: the agent runs the project commands with
	// `<Foreman> sandbox exec --data-dir <DataDir> <name>`.
	Foreman string
	DataDir string
}

// Commands returns the command lines that run the commands of p in the
// sandbox.
func (d Developer) Commands(p project.Config) []develop.AgentCommand {
	var out []develop.AgentCommand
	for _, c := range sandbox.Names(p) {
		out = append(out, develop.AgentCommand{Line: sandbox.Line(d.Foreman, d.DataDir, c.Name), Runs: strings.Join(c.Argv, " ")})
	}
	return out
}

// env is the environment of a development run, over the allowlist: the
// sandbox spec, a Bash timeout long enough for a test run, and git with
// an identity and only foreman's configuration over .git/config, so the
// agent's git runs no hook, pager, external diff or fsmonitor and signs
// nothing.
func (d Developer) env(spec string) []string {
	ms := strconv.Itoa(int(sandbox.AgentTimeout / time.Millisecond))
	env := []string{
		sandbox.EnvSpec + "=" + spec,
		"BASH_DEFAULT_TIMEOUT_MS=" + ms, "BASH_MAX_TIMEOUT_MS=" + ms,
		"GIT_AUTHOR_NAME=" + d.GitName, "GIT_AUTHOR_EMAIL=" + d.GitEmail,
		"GIT_COMMITTER_NAME=" + d.GitName, "GIT_COMMITTER_EMAIL=" + d.GitEmail,
	}
	return append(env, gitenv.Env()...)
}

// DevSession is the session of a development or revision run: its tools,
// rules and environment, with no prompt.
func (d Developer) DevSession(req develop.Request) Session {
	tools, allowed := DevTools(d.Commands(req.Project))
	return Session{
		Dir:       req.Workdir,
		PluginDir: req.SkillsDir,
		Tools:     tools,
		Allowed:   allowed,
		ExtraEnv:  d.env(req.SandboxSpec),
		MaxTurns:  DevMaxTurns,
		Timeout:   DevTimeout,
	}
}

// Develop runs a development or revision and validates the output.
func (d Developer) Develop(ctx context.Context, req develop.Request) (develop.Result, error) {
	if req.SandboxSpec == "" {
		return develop.Result{}, errors.New("no sandbox spec: the project commands would have nowhere to run")
	}
	req.Commands = d.Commands(req.Project)
	s := d.DevSession(req)
	s.Prompt = develop.Prompt(req)
	text, err := d.Claude.Exec(ctx, s)
	if err != nil {
		return develop.Result{}, err
	}
	r, err := develop.Parse(text)
	if err != nil {
		return develop.Result{}, &qualify.InvalidOutputError{Raw: text, Err: err}
	}
	return r, nil
}

// PreReview runs the foreman-prereview skill with read-only tools, in a
// fresh session.
func (d Developer) PreReview(ctx context.Context, req develop.PreReviewRequest) (develop.PreReviewResult, error) {
	text, err := d.Claude.Exec(ctx, Session{
		Dir:       req.Workdir,
		PluginDir: req.SkillsDir,
		Tools:     ReadOnlyTools,
		MaxTurns:  PreReviewMaxTurns,
		Timeout:   PreReviewTimeout,
		Prompt:    develop.PreReviewPrompt(req),
	})
	if err != nil {
		return develop.PreReviewResult{}, err
	}
	r, err := develop.ParsePreReview(text)
	if err != nil {
		return develop.PreReviewResult{}, &qualify.InvalidOutputError{Raw: text, Err: err}
	}
	return r, nil
}
