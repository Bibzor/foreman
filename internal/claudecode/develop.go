package claudecode

import (
	"context"
	"os"
	"time"

	"github.com/Bibzor/foreman/internal/develop"
	"github.com/Bibzor/foreman/internal/project"
	"github.com/Bibzor/foreman/internal/qualify"
)

// Limits of the development and pre-review runs.
const (
	DevMaxTurns       = 150
	DevTimeout        = 45 * time.Minute
	PreReviewMaxTurns = 40
	PreReviewTimeout  = 10 * time.Minute
)

// DevTools returns the tools of a development run and the rules that
// allow them. Files are read and written in the clone only; Bash runs
// only the project's commands, verbatim, and the git commands that make
// commits. Nothing pushes, calls gh or reaches the network on purpose:
// the project commands, though, run code the agent wrote.
func DevTools(p project.Config) (tools, allowed []string) {
	tools = []string{"Read", "Grep", "Glob", "Edit", "Write", "Skill", "Bash"}
	allowed = []string{"Read", "Grep", "Glob", "Edit", "Write", "Skill"}
	for _, c := range p.Commands() {
		allowed = append(allowed, "Bash("+c.Raw+")")
	}
	for _, g := range []string{"add", "commit", "status", "diff", "log"} {
		allowed = append(allowed, "Bash(git "+g+":*)")
	}
	return tools, allowed
}

// Developer is the Claude Code development and pre-review agent.
type Developer struct {
	Claude *Claude
	// GitName and GitEmail sign the agent's commits.
	GitName  string
	GitEmail string
}

// gitEnv is the git setup of a development run: an identity, and no
// configuration but foreman's, so the agent's commits run no hook and
// sign nothing whatever the local user's git configuration says.
func (d Developer) gitEnv() []string {
	return []string{
		"GIT_AUTHOR_NAME=" + d.GitName, "GIT_AUTHOR_EMAIL=" + d.GitEmail,
		"GIT_COMMITTER_NAME=" + d.GitName, "GIT_COMMITTER_EMAIL=" + d.GitEmail,
		"GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=" + os.DevNull,
		"GIT_CONFIG_COUNT=2",
		"GIT_CONFIG_KEY_0=core.hooksPath", "GIT_CONFIG_VALUE_0=" + os.DevNull,
		"GIT_CONFIG_KEY_1=commit.gpgSign", "GIT_CONFIG_VALUE_1=false",
	}
}

// Develop runs a development or revision and validates the output.
func (d Developer) Develop(ctx context.Context, req develop.Request) (develop.Result, error) {
	tools, allowed := DevTools(req.Project)
	text, err := d.Claude.Exec(ctx, Session{
		Dir:       req.Workdir,
		PluginDir: req.SkillsDir,
		Tools:     tools,
		Allowed:   allowed,
		ExtraEnv:  d.gitEnv(),
		MaxTurns:  DevMaxTurns,
		Timeout:   DevTimeout,
		Prompt:    develop.Prompt(req),
	})
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
