package orchestrator

import (
	"bytes"
	"context"
	"fmt"
	"strings"

	"github.com/Bibzor/foreman/internal/project"
	"github.com/Bibzor/foreman/internal/sandbox"
	"github.com/Bibzor/foreman/internal/store"
	"github.com/Bibzor/foreman/internal/workspace"
)

// sandboxExample is the section a project adds to develop with foreman.
const sandboxExample = "sandbox:\n" +
	"  image: golang:1.27        # pinned: a tag other than latest, or a digest\n" +
	"  setup: [go mod download]  # runs with network, on the default branch\n" +
	"  cache: [/go/pkg]          # filled by setup, read-only for the commands\n"

// prepareSandbox fills the issue's caches by running the project's setup
// in a sandbox, on a copy of the default branch, then writes the spec of
// the agent's sandbox in the plugin directory, outside the clone. A
// project with no sandbox section is blocked: its commands would have
// nowhere to run.
func (e *Engine) prepareSandbox(ctx context.Context, c *Case, dirs workspace.Dirs, p project.Config) (string, *Transition, error) {
	if p.Sandbox == nil {
		return "", &Transition{
			To:     store.Blocked,
			Reason: "project configuration: no sandbox section",
			Comment: fmt.Sprintf("**Blocked: `%s` on `%s` has no `sandbox` section.** foreman runs the project's tests and "+
				"lint only in a sandbox container, with no network. Add one, for example:\n\n```yaml\n%s```",
				project.File, dirs.BaseBranch, sandboxExample),
		}, nil
	}
	t, err := e.retried(ctx, c, "prepare the sandbox", func() error {
		tree, err := e.Workspace.Checkout(ctx, c.Repo, c.Issue.Number, dirs.Workdir, dirs.ProjectSHA)
		if err != nil {
			return err
		}
		defer func() { _ = sandbox.RemoveAll(tree) }() // best effort: removed again before the next setup
		s := sandbox.Setup(p, e.Config.DataDir, c.Repo, c.Issue.Number, tree)
		if err := sandbox.ResetCaches(e.Config.DataDir, c.Repo, c.Issue.Number, s.Caches); err != nil {
			return err
		}
		if len(s.Commands) == 0 {
			return nil
		}
		var out bytes.Buffer
		if err := e.Sandbox.Setup(ctx, s, &out); err != nil {
			return fmt.Errorf("%w\n%s", err, lastLines(out.String(), 1500))
		}
		return nil
	})
	if t != nil || err != nil {
		return "", t, err
	}
	spec, err := sandbox.Write(dirs.SkillsDir, sandbox.New(p, e.Config.DataDir, c.Repo, c.Issue.Number, dirs.Workdir, sandbox.AgentTimeout))
	return spec, nil, err
}

// cleanupSandbox removes what the agent's sandbox commands left running,
// such as a container whose client the agent's Bash timeout killed.
func (e *Engine) cleanupSandbox(ctx context.Context, c *Case) {
	if err := e.Sandbox.Cleanup(context.WithoutCancel(ctx), c.Repo, c.Issue.Number); err != nil {
		e.Log.Warn("sandbox cleanup failed", "repo", c.Repo.String(), "issue", c.Issue.Number, "err", err)
	}
}

func lastLines(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) > n {
		s = "..." + strings.ToValidUTF8(s[len(s)-n:], "")
	}
	return s
}
