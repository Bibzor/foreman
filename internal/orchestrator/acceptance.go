package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/Bibzor/foreman/internal/acceptance"
	"github.com/Bibzor/foreman/internal/github"
	"github.com/Bibzor/foreman/internal/project"
	"github.com/Bibzor/foreman/internal/qualify"
	"github.com/Bibzor/foreman/internal/store"
	"github.com/Bibzor/foreman/internal/workspace"
)

// MaxAcceptanceRounds is how many times foreman develops an issue again
// after an acceptance-ko, before a human must take over.
const MaxAcceptanceRounds = 2

const reasonAcceptanceLimit = "acceptance limit"

const checkAndLabel = "Check the change, then add `" + LabelAcceptanceOK + "` or `" + LabelAcceptanceKO +
	"` with a comment explaining what is wrong."

// merged checks that an authorized user approved the final commit of the
// pull request, then reads .foreman.yml at the tip of the default branch,
// so a configuration updated after the merge applies: with an acceptance
// section the change is deployed, otherwise a human checks it on their
// own.
func (e *Engine) merged(ctx context.Context, c *Case) (*Transition, error) {
	if c.Issue.PRNumber == 0 {
		return nil, errors.New("merged without a recorded pull request")
	}
	pr, err := e.GitHub.PullRequest(ctx, c.Repo, c.Issue.PRNumber)
	if err != nil {
		return nil, err
	}
	// An issue merged before foreman recorded merge commits has none.
	sha := c.Issue.MergeSHA
	if sha == "" {
		sha = pr.MergeCommitSHA
	}
	if sha == "" {
		return nil, fmt.Errorf("pull request #%d has no merge commit", pr.Number)
	}
	setSHA := func(is *store.Issue) { is.MergeSHA = sha }

	reviews, err := e.GitHub.Reviews(ctx, c.Repo, pr.Number)
	if err != nil {
		return nil, err
	}
	if why := e.unapproved(reviews, pr.HeadSHA); why != "" {
		t := blocked("merged without an approval of the final commit", fmt.Sprintf(
			"**Blocked: pull request #%d was merged without an approval of the final commit** (`%s`): %s. "+
				"foreman deploys only a change an authorized user approved as merged.", pr.Number, short(pr.HeadSHA), why))
		t.Update = setSHA
		return t, nil
	}
	// Only for .foreman.yml: deploying prepares its own clone, so a
	// restart or a retry starts clean.
	dirs, p, t, err := e.acceptanceWorkspace(ctx, c, sha)
	if t != nil || err != nil {
		if t != nil {
			t.Update = setSHA
		}
		return t, err
	}
	if p.Acceptance == nil {
		t := noEnvironment(dirs)
		t.Update = setSHA
		return t, nil
	}
	return &Transition{
		To:     store.Deploying,
		Reason: "approved and merged",
		Comment: fmt.Sprintf("**Deploying.** foreman deploys the merge commit `%s` to the acceptance environment, "+
			"configured by `%s` at `%s`.", short(sha), project.File, short(dirs.ProjectSHA)),
		Update: setSHA,
	}, nil
}

// noEnvironment goes to in-acceptance without deploying anything.
func noEnvironment(dirs workspace.Dirs) *Transition {
	return &Transition{
		To:     store.InAcceptance,
		Reason: "no acceptance environment",
		Comment: fmt.Sprintf("**In acceptance.** There is no automated acceptance environment (no `acceptance` section "+
			"in `%s` at `%s`, the tip of `%s`), so foreman deployed nothing. %s",
			project.File, short(dirs.ProjectSHA), dirs.BaseBranch, checkAndLabel),
	}
}

// unapproved returns why the reviews do not approve head, or "". As on
// GitHub, comments and pending reviews leave the approval state as it is;
// the last review by an authorized user that sets it must be an approval
// of head.
func (e *Engine) unapproved(reviews []github.Review, head string) string {
	var last *github.Review
	for i, r := range reviews {
		if e.isBot(r.Author) || !e.Config.IsAuthorized(r.Author) {
			continue
		}
		switch r.State {
		case "APPROVED", "CHANGES_REQUESTED", "DISMISSED":
			last = &reviews[i]
		}
	}
	switch {
	case last == nil:
		return "no authorized user approved it"
	case last.State != "APPROVED":
		return fmt.Sprintf("the last review by an authorized user, @%s, is %s", last.Author, strings.ToLower(last.State))
	case head == "" || last.CommitID != head:
		return fmt.Sprintf("@%s approved `%s`, an older commit", last.Author, short(last.CommitID))
	}
	return ""
}

// acceptanceWorkspace prepares a fresh clone at the merge commit, the
// code to deploy, and reads .foreman.yml at the tip of the default
// branch, which may be newer. No agent runs, so skills_version is not
// checked: a project that bumped it after the merge still deploys.
func (e *Engine) acceptanceWorkspace(ctx context.Context, c *Case, sha string) (workspace.Dirs, project.Config, *Transition, error) {
	var dirs workspace.Dirs
	if t, err := e.prepare(ctx, c, func() (err error) {
		dirs, err = e.Workspace.PrepareAt(ctx, c.Repo, c.Issue.Number, sha)
		return err
	}); t != nil || err != nil {
		return workspace.Dirs{}, project.Config{}, t, err
	}
	p, t := deployConfig(dirs)
	return dirs, p, t, nil
}

// deploying deploys the merge commit from a fresh clone and runs the
// smoke checks. A failed smoke check removes the deployment.
func (e *Engine) deploying(ctx context.Context, c *Case) (*Transition, error) {
	if e.Deployer == nil {
		return nil, errors.New("no acceptance deployer configured")
	}
	dirs, p, t, err := e.acceptanceWorkspace(ctx, c, c.Issue.MergeSHA)
	if t != nil || err != nil {
		return t, err
	}
	if p.Acceptance == nil { // removed from the default branch since merged ran
		return noEnvironment(dirs), nil
	}
	spec := acceptance.Spec{Repo: c.Repo, Issue: c.Issue.Number, SHA: c.Issue.MergeSHA, Dir: dirs.Workdir, Config: *p.Acceptance}
	dep, err := e.Deployer.Deploy(ctx, spec)
	if err == nil {
		if err = e.Deployer.Smoke(ctx, dep, p.Acceptance.Smoke); err != nil && ctx.Err() == nil {
			if serr := e.Deployer.Stop(ctx, dep); serr != nil {
				e.Log.Error("remove the failed deployment", "repo", c.Repo.String(), "issue", c.Issue.Number, "err", serr)
			}
		}
	}
	var se *acceptance.StageError
	switch {
	case err == nil:
	case ctx.Err() != nil:
		return nil, err
	case errors.Is(err, acceptance.ErrUnavailable):
		return e.retried(ctx, c, "reach the deployment backend", func() error { return err })
	case errors.As(err, &se):
		return blocked("acceptance "+se.Stage+" failed", fmt.Sprintf(
			"**Blocked: acceptance %s failed** for the merge commit `%s`. Nothing is deployed for this repository now. "+
				"The end of the log:\n\n%s", se.Stage, short(c.Issue.MergeSHA), qualify.Fenced(se.Log, maxDetail))), nil
	default:
		return nil, err
	}
	return &Transition{
		To:     store.InAcceptance,
		Reason: "deployed " + short(dep.SHA),
		Comment: fmt.Sprintf("**In acceptance.** foreman deployed the merge commit `%s` and its smoke checks pass: %s\n\n"+
			"- Deployed sha: `%s`\n- Configuration: `%s` at `%s`, the tip of `%s`\n\n%s",
			short(dep.SHA), dep.URL, dep.SHA, project.File, dirs.ProjectSHA, dirs.BaseBranch, checkAndLabel),
	}, nil
}

// inAcceptance waits for an authorized acceptance-ok, which closes the
// issue, or acceptance-ko with a comment, which develops it again on a
// new branch from the default branch. The merged PR is never reopened,
// and the deployment keeps running: it is the repository's shared
// acceptance environment.
func (e *Engine) inAcceptance(ctx context.Context, c *Case) (*Transition, error) {
	ok, err := c.Authorized(ctx, LabelAcceptanceOK)
	if err != nil {
		return nil, err
	}
	if ok {
		return &Transition{
			To:     store.Done,
			Reason: "acceptance-ok",
			Comment: fmt.Sprintf("**Done.** @%s accepted the change, so foreman closes this issue. "+
				"The acceptance environment keeps running.", c.labeler(LabelAcceptanceOK)),
			RemoveLabels: []string{LabelAcceptanceOK},
			Close:        true,
		}, nil
	}
	if ok, err = c.Authorized(ctx, LabelAcceptanceKO); err != nil || !ok {
		return nil, err
	}
	fb, found, err := c.acceptanceFeedback(ctx)
	if err != nil {
		return nil, err
	}
	if !found {
		// Keyed by the labeling event: once per acceptance-ko.
		added, err := e.Store.Notice(ctx, c.Repo.String(), c.Issue.Number, c.labelEvent(LabelAcceptanceKO).ID, fmt.Sprintf(
			"**Waiting for feedback.** `%s` needs a comment by an authorized user explaining what is wrong; "+
				"foreman develops the issue again from it.", LabelAcceptanceKO))
		if err != nil || !added {
			return nil, err
		}
		return nil, e.flush(ctx, c.Repo)
	}
	if c.Issue.AcceptanceRounds >= MaxAcceptanceRounds {
		return &Transition{
			To:     store.Blocked,
			Reason: reasonAcceptanceLimit,
			Comment: fmt.Sprintf("**Blocked: the change was rejected at acceptance again after %d rounds** (by @%s). "+
				"A human needs to take over.", MaxAcceptanceRounds, fb.Author),
			RemoveLabels: []string{LabelAcceptanceKO},
		}, nil
	}
	round := c.Issue.AcceptanceRounds + 1
	next := c.Issue
	next.AcceptanceRounds = round
	return &Transition{
		To:     store.Queued,
		Reason: fmt.Sprintf("acceptance-ko round %d", round),
		Comment: fmt.Sprintf("**Rejected at acceptance** by @%s (round %d of %d). foreman develops the issue again from that "+
			"comment, on a new branch `%s` from the default branch; pull request #%d stays merged.",
			fb.Author, round, MaxAcceptanceRounds, Branch(next), c.Issue.PRNumber),
		RemoveLabels: []string{LabelAcceptanceKO},
		Update: func(is *store.Issue) {
			is.AcceptanceRounds, is.AcceptanceFeedback, is.AcceptanceFeedbackAuthor = round, fb.Body, fb.Author
		},
	}, nil
}

// acceptanceFeedback is the newest comment by an authorized user since
// the comment announcing the current state.
func (c *Case) acceptanceFeedback(ctx context.Context) (github.Comment, bool, error) {
	since, ok, err := c.stateCommentTime(ctx)
	if err != nil || !ok {
		return github.Comment{}, false, err
	}
	cs, err := c.Comments(ctx)
	if err != nil {
		return github.Comment{}, false, err
	}
	for i := len(cs) - 1; i >= 0; i-- {
		cm := cs[i]
		if cm.CreatedAt.Before(since) {
			break
		}
		if !c.e.isBot(cm.Author) && c.e.Config.IsAuthorized(cm.Author) && strings.TrimSpace(cm.Body) != "" {
			return cm, true, nil
		}
	}
	return github.Comment{}, false, nil
}

// labelEvent is the latest "labeled" event of label, loaded by
// Authorized.
func (c *Case) labelEvent(label string) github.LabelEvent {
	var last github.LabelEvent
	for _, ev := range c.events {
		if strings.EqualFold(ev.Label, label) {
			last = ev
		}
	}
	return last
}

func (c *Case) labeler(label string) string { return c.labelEvent(label).Actor }

func short(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}
