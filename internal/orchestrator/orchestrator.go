// Package orchestrator drives tracked issues through the foreman state
// machine. The database is the source of truth; a foreman:* label on
// GitHub mirrors each issue's state.
//
// A polling cycle (Tick) flushes pending GitHub side effects, picks up new
// opt-ins, then runs the handler registered for each tracked issue's
// state. Adding a step, such as development from the queued state, is
// registering a Handler: the engine does not change.
package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/Bibzor/foreman/internal/config"
	"github.com/Bibzor/foreman/internal/github"
	"github.com/Bibzor/foreman/internal/qualify"
	"github.com/Bibzor/foreman/internal/store"
	"github.com/Bibzor/foreman/internal/workspace"
)

// Trigger and decision labels.
const (
	LabelTrigger = "foreman"
	LabelGo      = "go"
	LabelNoGo    = "no-go"
)

// GitHub is what the engine needs from GitHub.
type GitHub interface {
	BotLogin(ctx context.Context) (string, error)
	EnsureLabels(ctx context.Context, repo config.Repo, labels []github.Label) error
	OpenIssuesWithLabel(ctx context.Context, repo config.Repo, label string) ([]github.Issue, error)
	Issue(ctx context.Context, repo config.Repo, number int) (github.Issue, error)
	Comments(ctx context.Context, repo config.Repo, number int) ([]github.Comment, error)
	LabelEvents(ctx context.Context, repo config.Repo, number int) ([]github.LabelEvent, error)
	AddLabel(ctx context.Context, repo config.Repo, number int, label string) error
	RemoveLabel(ctx context.Context, repo config.Repo, number int, label string) error
	CreateComment(ctx context.Context, repo config.Repo, number int, body string) (int64, error)
	CloseIssue(ctx context.Context, repo config.Repo, number int) error
}

// Workspace prepares the directories an agent works in.
type Workspace interface {
	Prepare(ctx context.Context, repo config.Repo, number int) (workspace.Dirs, error)
}

// Handler handles a tracked issue in one state. It returns the transition
// to apply, or nil to leave the issue where it is.
type Handler interface {
	Handle(ctx context.Context, c *Case) (*Transition, error)
}

// HandlerFunc adapts a function to Handler.
type HandlerFunc func(ctx context.Context, c *Case) (*Transition, error)

// Handle calls f.
func (f HandlerFunc) Handle(ctx context.Context, c *Case) (*Transition, error) { return f(ctx, c) }

// Transition is a state change and the comment that announces it.
type Transition struct {
	To           store.State
	Reason       string // stored in the history
	Comment      string // posted on the issue
	RemoveLabels []string
	Close        bool
	Update       func(*store.Issue) // other fields of the new row
}

// Deps are the engine's collaborators.
type Deps struct {
	Config    config.Config
	GitHub    GitHub
	Store     *store.Store
	Workspace Workspace
	Agent     qualify.Agent
	Log       *slog.Logger
	Now       func() time.Time
}

// Engine runs the state machine.
type Engine struct {
	Deps
	handlers map[store.State]Handler
	bot      string
}

// New returns an engine with the qualification handlers registered.
func New(d Deps) *Engine {
	if d.Log == nil {
		d.Log = slog.New(slog.DiscardHandler)
	}
	if d.Now == nil {
		d.Now = time.Now
	}
	e := &Engine{Deps: d, handlers: make(map[store.State]Handler)}
	e.Register(store.Qualifying, HandlerFunc(e.qualifying))
	e.Register(store.NeedsInfo, HandlerFunc(e.needsInfo))
	e.Register(store.AwaitingGo, HandlerFunc(e.awaitingGo))
	e.Register(store.Suspect, HandlerFunc(e.suspect))
	return e
}

// Register sets the handler of a state.
func (e *Engine) Register(s store.State, h Handler) { e.handlers[s] = h }

// Labels are the labels foreman uses, created on start when missing.
func Labels() []github.Label {
	return []github.Label{
		{Name: LabelTrigger, Color: "5319e7", Description: "Hand this issue to foreman"},
		{Name: store.Qualifying.Label(), Color: "c5def5", Description: "foreman: qualifying"},
		{Name: store.NeedsInfo.Label(), Color: "fbca04", Description: "foreman: waiting for answers"},
		{Name: store.AwaitingGo.Label(), Color: "0e8a16", Description: "foreman: waiting for go / no-go"},
		{Name: store.Queued.Label(), Color: "1d76db", Description: "foreman: approved, queued"},
		{Name: store.Blocked.Label(), Color: "b60205", Description: "foreman: blocked, needs a human"},
		{Name: store.Suspect.Label(), Color: "d93f0b", Description: "foreman: suspicious, needs a human"},
		{Name: LabelGo, Color: "0e8a16", Description: "Approve (authorized users only)"},
		{Name: LabelNoGo, Color: "b60205", Description: "Reject (authorized users only)"},
	}
}

// Start prepares the engine: finds the bot login and creates the labels.
func (e *Engine) Start(ctx context.Context) error {
	bot, err := e.GitHub.BotLogin(ctx)
	if err != nil {
		return fmt.Errorf("find the bot login: %w", err)
	}
	e.bot = bot
	for _, repo := range e.Config.Repos {
		if err := e.GitHub.EnsureLabels(ctx, repo, Labels()); err != nil {
			return fmt.Errorf("%s: ensure labels: %w", repo, err)
		}
	}
	return nil
}

// Run calls Start, then Tick every poll interval until ctx is done.
func (e *Engine) Run(ctx context.Context) error {
	if err := e.Start(ctx); err != nil {
		return err
	}
	t := time.NewTicker(e.Config.PollInterval)
	defer t.Stop()
	for {
		if err := e.Tick(ctx); err != nil && ctx.Err() == nil {
			e.Log.Error("cycle failed", "err", err)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
		}
	}
}

// Tick runs one polling cycle over every repository. An error on one
// issue does not stop the others; all errors are returned joined.
func (e *Engine) Tick(ctx context.Context) error {
	var errs []error
	for _, repo := range e.Config.Repos {
		if err := e.tickRepo(ctx, repo); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", repo, err))
		}
	}
	return errors.Join(errs...)
}

func (e *Engine) tickRepo(ctx context.Context, repo config.Repo) error {
	// While GitHub side effects cannot be applied, stop: they must keep
	// their order, and an agent run whose result cannot be posted is waste.
	if err := e.flush(ctx, repo); err != nil {
		return err
	}
	var errs []error
	if err := e.optIns(ctx, repo); err != nil {
		if errors.Is(err, errOutbox) {
			return err
		}
		errs = append(errs, err)
	}
	tracked, err := e.Store.Tracked(ctx, repo.String())
	if err != nil {
		return errors.Join(append(errs, err)...)
	}
	for _, is := range tracked {
		if err := e.process(ctx, repo, is); err != nil {
			errs = append(errs, fmt.Errorf("#%d: %w", is.Number, err))
		}
		if ctx.Err() != nil || errors.Is(err, errOutbox) {
			break
		}
	}
	return errors.Join(errs...)
}

// optIns starts tracking open issues labeled foreman by an authorized user.
func (e *Engine) optIns(ctx context.Context, repo config.Repo) error {
	issues, err := e.GitHub.OpenIssuesWithLabel(ctx, repo, LabelTrigger)
	if err != nil {
		return err
	}
	var errs []error
	for _, live := range issues {
		if _, ok, err := e.Store.Get(ctx, repo.String(), live.Number); err != nil || ok {
			errs = append(errs, err)
			continue
		}
		c := &Case{Repo: repo, Live: live, e: e,
			Issue: store.Issue{Repo: repo.String(), Number: live.Number, Author: live.Author}}
		ok, err := c.Authorized(ctx, LabelTrigger)
		if err != nil || !ok {
			errs = append(errs, err)
			continue
		}
		err = e.apply(ctx, c, &Transition{
			To:      store.Qualifying,
			Reason:  "opt-in",
			Comment: "foreman picked up this issue and is qualifying it.",
		})
		if errors.Is(err, errOutbox) {
			return errors.Join(append(errs, err)...)
		}
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

// maxSteps bounds the transitions of one issue in one cycle, so that, for
// example, an answered question is qualified without waiting a cycle.
const maxSteps = 3

// process runs one tracked issue through its state handlers.
func (e *Engine) process(ctx context.Context, repo config.Repo, is store.Issue) error {
	for range maxSteps {
		// Read the issue again after each step: the previous one changed
		// its labels.
		live, err := e.GitHub.Issue(ctx, repo, is.Number)
		if err != nil {
			return err
		}
		c := &Case{Repo: repo, Issue: is, Live: live, e: e}
		var t *Transition
		if !live.Open {
			t = &Transition{
				To:      store.Closed,
				Reason:  "closed_by_human",
				Comment: "This issue was closed, so foreman stops tracking it.",
			}
		} else if h, ok := e.handlers[is.State]; ok {
			if t, err = h.Handle(ctx, c); err != nil {
				return err
			}
		}
		if t == nil {
			return nil
		}
		if err := e.apply(ctx, c, t); err != nil {
			return err
		}
		is = c.Issue
		if is.State == store.Closed {
			return nil
		}
	}
	return nil
}

// apply stores the transition with its side effects, then flushes them.
func (e *Engine) apply(ctx context.Context, c *Case, t *Transition) error {
	next := c.Issue
	next.State = t.To
	next.StateEnteredAt = e.Now().UTC()
	next.PrepFailures = 0
	if t.Update != nil {
		t.Update(&next)
	}
	next, err := e.Store.Transition(ctx, c.Issue.State, next, t.Reason, store.Effect{
		Comment:      t.Comment,
		RemoveLabels: t.RemoveLabels,
		Close:        t.Close,
	})
	if err != nil {
		return err
	}
	e.Log.Info("transition", "repo", c.Repo.String(), "issue", next.Number, "from", string(c.Issue.State), "to", string(t.To), "reason", t.Reason)
	c.Issue = next
	return e.flush(ctx, c.Repo)
}

// marker tags each bot comment with its outbox entry, so a comment posted
// just before a crash is recognized instead of posted twice.
func marker(id int64) string { return fmt.Sprintf("<!-- foreman:outbox:%d -->", id) }

// errOutbox marks a failure to apply pending GitHub side effects.
var errOutbox = errors.New("outbox")

// flush applies pending side effects in order and stops at the first
// failure, to keep that order on the next attempt.
func (e *Engine) flush(ctx context.Context, repo config.Repo) error {
	pending, err := e.Store.Pending(ctx, repo.String())
	if err != nil {
		return fmt.Errorf("%w: %w", errOutbox, err)
	}
	for _, o := range pending {
		if err := e.effect(ctx, repo, o); err != nil {
			return fmt.Errorf("%w: #%d: %w", errOutbox, o.Number, err)
		}
	}
	return nil
}

func (e *Engine) effect(ctx context.Context, repo config.Repo, o store.Outbox) error {
	if o.Comment != "" && !o.CommentPosted {
		if err := e.postOnce(ctx, repo, o); err != nil {
			return err
		}
		if err := e.Store.MarkCommentPosted(ctx, o.ID); err != nil {
			return err
		}
	}
	for _, l := range o.RemoveLabels {
		if err := e.GitHub.RemoveLabel(ctx, repo, o.Number, l); err != nil {
			return err
		}
	}
	if o.SyncLabel {
		if err := e.syncLabel(ctx, repo, o.Number, o.State); err != nil {
			return err
		}
	}
	if o.Close {
		if err := e.GitHub.CloseIssue(ctx, repo, o.Number); err != nil {
			return err
		}
	}
	return e.Store.MarkDone(ctx, o.ID)
}

func (e *Engine) postOnce(ctx context.Context, repo config.Repo, o store.Outbox) error {
	mark := marker(o.ID)
	comments, err := e.GitHub.Comments(ctx, repo, o.Number)
	if err != nil {
		return err
	}
	for _, c := range comments {
		if e.isBot(c.Author) && strings.Contains(c.Body, mark) {
			return nil
		}
	}
	_, err = e.GitHub.CreateComment(ctx, repo, o.Number, o.Comment+"\n\n"+mark)
	return err
}

// syncLabel makes the foreman:* labels of the issue exactly the label of s.
func (e *Engine) syncLabel(ctx context.Context, repo config.Repo, number int, s store.State) error {
	live, err := e.GitHub.Issue(ctx, repo, number)
	if err != nil {
		return err
	}
	want := s.Label()
	for _, l := range live.Labels {
		if strings.HasPrefix(l, LabelTrigger+":") && !strings.EqualFold(l, want) {
			if err := e.GitHub.RemoveLabel(ctx, repo, number, l); err != nil {
				return err
			}
		}
	}
	if want != "" && !live.HasLabel(want) {
		return e.GitHub.AddLabel(ctx, repo, number, want)
	}
	return nil
}

func (e *Engine) isBot(login string) bool { return strings.EqualFold(login, e.bot) }

// Case is a tracked issue being handled: its stored row, its live GitHub
// view, and helpers that fetch more on demand.
type Case struct {
	Repo  config.Repo
	Issue store.Issue
	Live  github.Issue

	e        *Engine
	comments []github.Comment
	events   []github.LabelEvent
}

// Comments returns the issue comments, fetched once per cycle.
func (c *Case) Comments(ctx context.Context) ([]github.Comment, error) {
	if c.comments != nil {
		return c.comments, nil
	}
	cs, err := c.e.GitHub.Comments(ctx, c.Repo, c.Issue.Number)
	if err != nil {
		return nil, err
	}
	c.comments = append([]github.Comment{}, cs...)
	return c.comments, nil
}

// Snapshot is the live content of the issue, without the bot's comments.
func (c *Case) Snapshot(ctx context.Context) (qualify.Snapshot, error) {
	cs, err := c.Comments(ctx)
	if err != nil {
		return qualify.Snapshot{}, err
	}
	s := qualify.Snapshot{
		Repo: c.Repo.String(), Number: c.Live.Number,
		Title: c.Live.Title, Author: c.Live.Author, Body: c.Live.Body,
		Comments: []qualify.SnapshotComment{},
	}
	for _, cm := range cs {
		if !c.e.isBot(cm.Author) {
			s.Comments = append(s.Comments, qualify.SnapshotComment{ID: cm.ID, Author: cm.Author, Body: cm.Body})
		}
	}
	return s, nil
}

// Authorized reports whether label is on the issue and its latest
// "labeled" event is by an authorized user. For go and no-go the event
// must also be newer than the comment announcing the current state, so a
// stale approval cannot carry over; both times come from GitHub's clock.
// A label added by anyone else is ignored, and the bot says so once per
// labeling event.
func (c *Case) Authorized(ctx context.Context, label string) (bool, error) {
	if !c.Live.HasLabel(label) {
		return false, nil
	}
	if c.events == nil {
		evs, err := c.e.GitHub.LabelEvents(ctx, c.Repo, c.Issue.Number)
		if err != nil {
			return false, err
		}
		c.events = append([]github.LabelEvent{}, evs...)
	}
	var last *github.LabelEvent
	for i := range c.events {
		if strings.EqualFold(c.events[i].Label, label) {
			last = &c.events[i]
		}
	}
	if last == nil {
		return false, nil
	}
	if !c.e.Config.IsAuthorized(last.Actor) {
		_, err := c.e.Store.Notice(ctx, c.Repo.String(), c.Issue.Number, last.ID, fmt.Sprintf(
			"The label `%s` was added by @%s, who is not an authorized user, so foreman ignores it. "+
				"An authorized user can remove it and add it again.", label, last.Actor))
		if err != nil {
			return false, err
		}
		return false, c.e.flush(ctx, c.Repo)
	}
	if label == LabelTrigger {
		return true, nil
	}
	since, ok, err := c.stateCommentTime(ctx)
	if err != nil || !ok {
		return false, err
	}
	return !last.CreatedAt.Before(since), nil
}

// stateCommentTime returns when GitHub recorded the bot comment announcing
// the current state. ok is false while that comment is not posted.
func (c *Case) stateCommentTime(ctx context.Context) (time.Time, bool, error) {
	cs, err := c.Comments(ctx)
	if err != nil {
		return time.Time{}, false, err
	}
	mark := marker(c.Issue.StateOutboxID)
	for _, cm := range cs {
		if c.e.isBot(cm.Author) && strings.Contains(cm.Body, mark) {
			return cm.CreatedAt, true, nil
		}
	}
	return time.Time{}, false, nil
}
