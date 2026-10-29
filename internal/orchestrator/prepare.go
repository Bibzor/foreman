package orchestrator

import (
	"context"
	"errors"
	"fmt"

	"github.com/Bibzor/foreman/internal/qualify"
	"github.com/Bibzor/foreman/internal/store"
)

// MaxPrepFailures is how many consecutive workspace preparation failures
// an issue gets before a human must look at it.
const MaxPrepFailures = 5

// prepare runs fn, which prepares a workspace. A failure is returned, to
// be retried on the next cycle, until MaxPrepFailures in a row: then the
// issue is blocked with the last error. Errors are already redacted.
func (e *Engine) prepare(ctx context.Context, c *Case, fn func() error) (*Transition, error) {
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
		return nil, fmt.Errorf("prepare workspace (failure %d of %d): %w", n, MaxPrepFailures, err)
	}
	e.Log.Error("workspace preparation keeps failing", "repo", c.Repo.String(), "issue", c.Issue.Number, "failures", n, "err", err)
	return &Transition{
		To:     store.Blocked,
		Reason: fmt.Sprintf("workspace preparation failed %d times in a row: %v", n, err),
		Comment: fmt.Sprintf("**Blocked: foreman could not prepare a workspace %d times in a row.** The last error:\n\n%s",
			n, qualify.Fenced(err.Error(), 2000)),
	}, nil
}
