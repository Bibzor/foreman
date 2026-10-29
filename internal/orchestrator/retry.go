package orchestrator

import (
	"context"
	"fmt"

	"github.com/Bibzor/foreman/internal/store"
)

// retry handles a blocked issue: an authorized `retry`, added after the
// issue was blocked, sends it back to the state it was blocked from, which
// runs that step again from a fresh workspace.
func (e *Engine) retry(ctx context.Context, c *Case) (*Transition, error) {
	ok, err := c.Authorized(ctx, LabelRetry)
	if err != nil || !ok {
		return nil, err
	}
	hist, err := e.Store.History(ctx, c.Repo.String(), c.Issue.Number)
	if err != nil {
		return nil, err
	}
	var from store.State
	for i := len(hist) - 1; i >= 0; i-- {
		if hist[i].To == store.Blocked {
			from = hist[i].From
			break
		}
	}
	if _, ok := e.handlers[from]; !ok || from == store.Blocked {
		e.Log.Warn("retry: no step to run again", "repo", c.Repo.String(), "issue", c.Issue.Number, "from", string(from))
		return nil, nil
	}
	return &Transition{
		To:     from,
		Reason: "retry",
		Comment: fmt.Sprintf("**Retrying.** An authorized user added `%s`: foreman goes back to `%s` "+
			"and runs that step again from a fresh workspace.", LabelRetry, from),
		RemoveLabels: []string{LabelRetry},
	}, nil
}
