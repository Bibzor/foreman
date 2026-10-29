package orchestrator

import (
	"context"
	"errors"
	"fmt"

	"github.com/Bibzor/foreman/internal/qualify"
	"github.com/Bibzor/foreman/internal/store"
)

// MaxPrepFailures is how many consecutive failures to prepare a
// workspace, or to push from one, an issue gets before a human must look
// at it.
const MaxPrepFailures = 5

// prepare runs fn, which prepares a workspace. A failure is returned, to
// be retried on the next cycle, until MaxPrepFailures in a row: then the
// issue is blocked with the last error. Errors are already redacted.
func (e *Engine) prepare(ctx context.Context, c *Case, fn func() error) (*Transition, error) {
	return e.retried(ctx, c, "prepare a workspace", fn)
}

// retried is prepare for any step whose failure is worth retrying: what
// names it in the blocking comment.
func (e *Engine) retried(ctx context.Context, c *Case, what string, fn func() error) (*Transition, error) {
	err := fn()
	if err == nil {
		return nil, nil
	}
	if ctx.Err() != nil {
		return nil, err // shutting down: not the workspace's fault
	}
	n, serr := e.Store.PrepFailed(ctx, c.Repo.String(), c.Issue.Number)
	if serr != nil {
		return nil, errors.Join(err, serr)
	}
	if n < MaxPrepFailures {
		return nil, fmt.Errorf("%s (failure %d of %d): %w", what, n, MaxPrepFailures, err)
	}
	e.Log.Error("step keeps failing", "repo", c.Repo.String(), "issue", c.Issue.Number, "step", what, "failures", n, "err", err)
	return &Transition{
		To:     store.Blocked,
		Reason: fmt.Sprintf("could not %s %d times in a row: %v", what, n, err),
		Comment: fmt.Sprintf("**Blocked: foreman could not %s %d times in a row.** The last error:\n\n%s",
			what, n, qualify.Fenced(err.Error(), 2000)),
	}, nil
}
