package orchestrator

import (
	"context"
	"fmt"
	"strings"

	"github.com/Bibzor/foreman/internal/store"
)

// retry handles a blocked issue: an authorized `retry`, added after the
// issue was blocked, sends it back to the state it was blocked from, which
// runs that step again from a fresh workspace. Development goes back
// through queued, so the WIP limit applies. A block caused by a limit
// resets the counter that reached it.
func (e *Engine) retry(ctx context.Context, c *Case) (*Transition, error) {
	ok, err := c.Authorized(ctx, LabelRetry)
	if err != nil || !ok {
		return nil, err
	}
	hist, err := e.Store.History(ctx, c.Repo.String(), c.Issue.Number)
	if err != nil {
		return nil, err
	}
	var block store.Transition
	for i := len(hist) - 1; i >= 0; i-- {
		if hist[i].To == store.Blocked {
			block = hist[i]
			break
		}
	}
	from := block.From
	if _, ok := e.handlers[from]; !ok || from == store.Blocked {
		e.Log.Warn("retry: no step to run again", "repo", c.Repo.String(), "issue", c.Issue.Number, "from", string(from))
		return nil, nil
	}
	to, then := from, "runs that step again from a fresh workspace"
	if from == store.InProgress {
		to, then = store.Queued, "development starts again from a fresh workspace when the WIP limit allows"
	}
	t := &Transition{
		To:           to,
		Reason:       "retry",
		Comment:      fmt.Sprintf("**Retrying.** An authorized user added `%s`: foreman goes back to `%s` and %s.", LabelRetry, to, then),
		RemoveLabels: []string{LabelRetry},
	}
	if counter, reset := limitCounter(block.Reason); counter != "" {
		t.Update = reset
		t.Comment += fmt.Sprintf(" The `%s` counter was reset.", counter)
	}
	return t, nil
}

// limitCounter returns the counter whose limit caused a block with this
// reason, and how to reset it; "" when the block was not caused by a
// limit. prep_failures is reset by every transition already.
func limitCounter(reason string) (string, func(*store.Issue)) {
	switch {
	case reason == reasonNeedsInfoLimit:
		return "needs_info_rounds", func(is *store.Issue) { is.NeedsInfoRounds = 0 }
	case reason == reasonRevisionLimit:
		return "revision_rounds", func(is *store.Issue) { is.RevisionRounds = 0 }
	case strings.HasPrefix(reason, reasonPrepPrefix):
		return "prep_failures", nil
	}
	return "", nil
}
