package orchestrator

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/Bibzor/foreman/internal/qualify"
	"github.com/Bibzor/foreman/internal/store"
	"github.com/Bibzor/foreman/internal/workspace"
)

// MaxNeedsInfoRounds is how many rounds of questions an issue gets before
// a human must take over.
const MaxNeedsInfoRounds = 3

const reasonNeedsInfoLimit = "needs_info_limit"

// qualifying runs the agent on a fresh workspace and routes its result.
func (e *Engine) qualifying(ctx context.Context, c *Case) (*Transition, error) {
	snap, err := c.Snapshot(ctx)
	if err != nil {
		return nil, err
	}
	var dirs workspace.Dirs
	if t, err := e.prepare(ctx, c, func() (err error) {
		dirs, err = e.Workspace.Prepare(ctx, c.Repo, c.Issue.Number)
		return err
	}); t != nil || err != nil {
		return t, err
	}
	req := qualify.Request{Workdir: dirs.Workdir, SkillsDir: dirs.SkillsDir, Issue: snap}

	res, err := e.Agent.Qualify(ctx, req)
	var inv *qualify.InvalidOutputError
	if errors.As(err, &inv) {
		first := inv.Raw
		e.Log.Warn("invalid agent output, retrying", "repo", c.Repo.String(), "issue", c.Issue.Number, "err", inv.Err)
		res, err = e.Agent.Qualify(ctx, req)
		if errors.As(err, &inv) {
			path, serr := e.saveRaw(c, first, inv.Raw)
			if serr != nil {
				return nil, serr
			}
			e.Log.Error("invalid agent output twice", "repo", c.Repo.String(), "issue", c.Issue.Number, "err", inv.Err, "saved", path)
			return &Transition{
				To:      store.Blocked,
				Reason:  "invalid agent output: " + inv.Err.Error(),
				Comment: "**Blocked: qualification failed.** The agent's output was invalid twice. A maintainer can find it in the foreman data directory.",
			}, nil
		}
	}
	if err != nil {
		if ctx.Err() != nil {
			return nil, err // shutting down: qualify again after restart
		}
		e.Log.Error("agent failed", "repo", c.Repo.String(), "issue", c.Issue.Number, "err", err)
		return &Transition{
			To:      store.Blocked,
			Reason:  "agent failure: " + err.Error(),
			Comment: "**Blocked: qualification failed.** The agent did not complete. A maintainer can find details in the foreman logs.",
		}, nil
	}

	// The deterministic checks win over the agent.
	for _, f := range qualify.Scan(snap) {
		res.Suspicious = true
		res.SuspiciousReasons = append(res.SuspiciousReasons, f.String())
	}
	data, sha := snap.Canonical()
	stored, err := json.Marshal(res)
	if err != nil {
		return nil, err
	}
	keep := func(is *store.Issue) {
		is.Qualification, is.Snapshot, is.SnapshotSHA = string(stored), data, sha
	}
	if res.Suspicious {
		return &Transition{
			To:           store.Suspect,
			Reason:       "suspicious: " + strings.Join(res.SuspiciousReasons, "; "),
			Comment:      qualify.SuspectComment(res.SuspiciousReasons, snap),
			RemoveLabels: []string{LabelGo, LabelNoGo},
			Update:       keep,
		}, nil
	}
	t := route(c.Issue, res, snap, "")
	prev := t.Update
	t.Update = func(is *store.Issue) {
		keep(is)
		if prev != nil {
			prev(is)
		}
	}
	return t, nil
}

// route turns a qualification result into its transition. The agent
// never closes an issue: out_of_scope is a no-go recommendation.
func route(is store.Issue, res qualify.Result, snap qualify.Snapshot, intro string) *Transition {
	switch res.Decision {
	case qualify.NeedsInfo:
		if is.NeedsInfoRounds >= MaxNeedsInfoRounds {
			return &Transition{
				To:     store.Blocked,
				Reason: reasonNeedsInfoLimit,
				Comment: intro + fmt.Sprintf("**Blocked: still unclear after %d rounds of questions.** "+
					"A human needs to take over.", MaxNeedsInfoRounds),
			}
		}
		round := is.NeedsInfoRounds + 1
		return &Transition{
			To:      store.NeedsInfo,
			Reason:  "needs_info round " + strconv.Itoa(round),
			Comment: intro + qualify.NeedsInfoComment(res, round, MaxNeedsInfoRounds),
			Update:  func(is *store.Issue) { is.NeedsInfoRounds = round },
		}
	case qualify.OutOfScope:
		return &Transition{
			To:           store.AwaitingGo,
			Reason:       "out_of_scope: recommend no-go",
			Comment:      intro + qualify.AwaitingGoComment(res, snap),
			RemoveLabels: []string{LabelGo, LabelNoGo},
		}
	default:
		return &Transition{
			To:           store.AwaitingGo,
			Reason:       "ready",
			Comment:      intro + qualify.AwaitingGoComment(res, snap),
			RemoveLabels: []string{LabelGo, LabelNoGo},
		}
	}
}

// saveRaw writes the invalid outputs under data_dir/agent-output, readable
// by the local user only. They are never posted on GitHub.
func (e *Engine) saveRaw(c *Case, outputs ...string) (string, error) {
	dir := filepath.Join(e.Config.DataDir, "agent-output", c.Repo.Owner, c.Repo.Name)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	path := filepath.Join(dir, fmt.Sprintf("%d-%s.txt", c.Issue.Number, e.Now().UTC().Format("20060102T150405.000000000Z")))
	var b strings.Builder
	for i, o := range outputs {
		fmt.Fprintf(&b, "===== attempt %d =====\n%s\n", i+1, o)
	}
	return path, os.WriteFile(path, []byte(b.String()), 0o600)
}

// needsInfo goes back to qualifying when the issue author or an authorized
// user comments with something the last qualification did not see.
func (e *Engine) needsInfo(ctx context.Context, c *Case) (*Transition, error) {
	snap, err := qualify.ParseSnapshot(c.Issue.Snapshot)
	if err != nil {
		return nil, fmt.Errorf("stored snapshot: %w", err)
	}
	seen := make(map[int64]bool, len(snap.Comments))
	for _, cm := range snap.Comments {
		seen[cm.ID] = true
	}
	comments, err := c.Comments(ctx)
	if err != nil {
		return nil, err
	}
	for _, cm := range comments {
		if seen[cm.ID] || e.isBot(cm.Author) {
			continue
		}
		if strings.EqualFold(cm.Author, c.Issue.Author) || e.Config.IsAuthorized(cm.Author) {
			return &Transition{
				To:      store.Qualifying,
				Reason:  "answer from " + cm.Author,
				Comment: "Thanks @" + cm.Author + ", qualifying again with the new information.",
			}, nil
		}
	}
	return nil, nil
}

// awaitingGo applies an authorized no-go (close) or go (queue, if the
// issue is still what was qualified).
func (e *Engine) awaitingGo(ctx context.Context, c *Case) (*Transition, error) {
	if ok, err := c.Authorized(ctx, LabelNoGo); err != nil || ok {
		if err != nil {
			return nil, err
		}
		return &Transition{
			To:      store.Closed,
			Reason:  "no-go",
			Comment: "Closed: an authorized user chose `no-go`.",
			Close:   true,
		}, nil
	}
	ok, err := c.Authorized(ctx, LabelGo)
	if err != nil || !ok {
		return nil, err
	}
	if t, err := e.changedSinceQualification(ctx, c); err != nil || t != nil {
		return t, err
	}
	return &Transition{
		To:     store.Queued,
		Reason: "go",
		Comment: fmt.Sprintf("**Queued.** Approved with `go`. Development will work from the issue as qualified "+
			"(snapshot sha256 `%s`), not from later edits.", c.Issue.SnapshotSHA),
		RemoveLabels: []string{LabelGo},
	}, nil
}

// suspect: an authorized go means "use the qualification anyway", as long
// as the issue has not changed since.
func (e *Engine) suspect(ctx context.Context, c *Case) (*Transition, error) {
	ok, err := c.Authorized(ctx, LabelGo)
	if err != nil || !ok {
		return nil, err
	}
	if t, err := e.changedSinceQualification(ctx, c); err != nil || t != nil {
		return t, err
	}
	var res qualify.Result
	if err := json.Unmarshal([]byte(c.Issue.Qualification), &res); err != nil {
		return nil, fmt.Errorf("stored qualification: %w", err)
	}
	snap, err := qualify.ParseSnapshot(c.Issue.Snapshot)
	if err != nil {
		return nil, fmt.Errorf("stored snapshot: %w", err)
	}
	t := route(c.Issue, res, snap, "An authorized user reviewed the suspicious content and chose `go`.\n\n")
	t.Reason = "suspect overridden: " + t.Reason
	t.RemoveLabels = []string{LabelGo, LabelNoGo}
	return t, nil
}

// changedSinceQualification binds a go to the content that was qualified:
// if the title, body or any non-bot comment changed, the go is not applied
// and the issue is qualified again.
func (e *Engine) changedSinceQualification(ctx context.Context, c *Case) (*Transition, error) {
	live, err := c.Snapshot(ctx)
	if err != nil {
		return nil, err
	}
	if _, sha := live.Canonical(); sha == c.Issue.SnapshotSHA {
		return nil, nil
	}
	return &Transition{
		To:     store.Qualifying,
		Reason: "issue changed after qualification",
		Comment: "The issue changed after qualification (title, body or comments), so the `go` was not applied. " +
			"Qualifying again; a new `go` will be needed.",
		RemoveLabels: []string{LabelGo},
	}, nil
}
