package orchestrator

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/Bibzor/foreman/internal/develop"
	"github.com/Bibzor/foreman/internal/github"
	"github.com/Bibzor/foreman/internal/project"
	"github.com/Bibzor/foreman/internal/qualify"
	"github.com/Bibzor/foreman/internal/store"
	"github.com/Bibzor/foreman/internal/workspace"
	"github.com/Bibzor/foreman/skills"
)

// MaxRevisions is how many times foreman revises a pull request after an
// authorized user requested changes, before a human must take over.
const MaxRevisions = 2

const reasonRevisionLimit = "revision limit"

// Limits that keep comments and PR bodies under GitHub's 65536 characters.
const (
	maxSection = 25000
	maxDetail  = 3000
	maxDiff    = 200000
	maxNotes   = 10000
)

// Branch is the branch foreman develops an issue on: foreman/<n>, then
// foreman/<n>-r<k> for the development restarted after the k-th
// acceptance-ko, from the default branch that holds the merged work.
func Branch(is store.Issue) string {
	b := "foreman/" + strconv.Itoa(is.Number)
	if is.AcceptanceRounds > 0 {
		b += "-r" + strconv.Itoa(is.AcceptanceRounds)
	}
	return b
}

// queued starts development when fewer than wip_limit issues, across all
// repositories, are in development or in review. Otherwise it says why
// once and waits.
func (e *Engine) queued(ctx context.Context, c *Case) (*Transition, error) {
	n, err := e.Store.CountInStates(ctx, store.InProgress, store.InReview)
	if err != nil {
		return nil, err
	}
	if n >= e.Config.WIPLimit {
		// Keyed by the queued transition: once per stay in queued.
		added, err := e.Store.Notice(ctx, c.Repo.String(), c.Issue.Number, -c.Issue.StateOutboxID, fmt.Sprintf(
			"**Waiting.** %d issues are already in development or in review, the limit (`wip_limit`) is %d. "+
				"Development starts when one of them is done.", n, e.Config.WIPLimit))
		if err != nil || !added {
			return nil, err
		}
		return nil, e.flush(ctx, c.Repo)
	}
	return &Transition{
		To:      store.InProgress,
		Reason:  "development started",
		Comment: fmt.Sprintf("**In progress.** Development started on the branch `%s`.", Branch(c.Issue)),
	}, nil
}

// approved is what development works from: the snapshot and the
// qualification that were approved, never the live issue.
func approved(is store.Issue) (qualify.Snapshot, qualify.Result, error) {
	snap, err := qualify.ParseSnapshot(is.Snapshot)
	if err != nil {
		return qualify.Snapshot{}, qualify.Result{}, fmt.Errorf("stored snapshot: %w", err)
	}
	var q qualify.Result
	if err := json.Unmarshal([]byte(is.Qualification), &q); err != nil {
		return qualify.Snapshot{}, qualify.Result{}, fmt.Errorf("stored qualification: %w", err)
	}
	return snap, q, nil
}

// projectConfig parses .foreman.yml for a step that runs an agent, so
// skills_version must be the pack foreman ships; a missing or invalid
// file blocks the issue.
func projectConfig(dirs workspace.Dirs) (project.Config, *Transition) {
	p, err := project.Parse(dirs.ProjectFile, skills.PackVersion)
	return configOrBlock(dirs, p, err)
}

// deployConfig parses .foreman.yml for a step that runs no agent:
// skills_version may pin any version.
func deployConfig(dirs workspace.Dirs) (project.Config, *Transition) {
	p, err := project.ParseNoPack(dirs.ProjectFile)
	return configOrBlock(dirs, p, err)
}

func configOrBlock(dirs workspace.Dirs, p project.Config, err error) (project.Config, *Transition) {
	if err == nil {
		return p, nil
	}
	return project.Config{}, &Transition{
		To:     store.Blocked,
		Reason: "project configuration: " + err.Error(),
		Comment: fmt.Sprintf("**Blocked: `%s` on `%s` is missing or invalid.**\n\n%s",
			project.File, dirs.BaseBranch, qualify.Fenced(err.Error(), maxDetail)),
	}
}

func blocked(reason, comment string) *Transition {
	return &Transition{To: store.Blocked, Reason: reason, Comment: comment}
}

// inProgress develops the issue on a fresh clone, checks the result
// without trusting the agent, pushes it and opens the pull request.
func (e *Engine) inProgress(ctx context.Context, c *Case) (*Transition, error) {
	snap, q, err := approved(c.Issue)
	if err != nil {
		return nil, err
	}
	branch := Branch(c.Issue)
	var dirs workspace.Dirs
	if t, err := e.prepare(ctx, c, func() (err error) {
		dirs, err = e.Workspace.PrepareDev(ctx, c.Repo, c.Issue.Number, branch)
		return err
	}); t != nil || err != nil {
		return t, err
	}
	p, t := projectConfig(dirs)
	if t != nil {
		return t, nil
	}
	req := develop.Request{
		Workdir: dirs.Workdir, SkillsDir: dirs.SkillsDir, Branch: branch, Project: p,
		Issue: snap, Summary: q.Summary, AcceptanceCriteria: q.AcceptanceCriteria,
	}
	if c.Issue.AcceptanceRounds > 0 {
		req.Feedback = &develop.Feedback{Author: c.Issue.AcceptanceFeedbackAuthor, Body: c.Issue.AcceptanceFeedback}
	}
	res, t, err := e.develop(ctx, c, req, "development")
	if t != nil || err != nil {
		return t, err
	}
	dir, tip, _, t, err := e.verify(ctx, c, dirs.Workdir, branch, p, develop.Range{Base: dirs.Base})
	if t != nil || err != nil {
		return t, err
	}

	// A pull request from this branch that foreman did not record would
	// make opening one fail forever.
	prs, err := e.GitHub.PullRequestsByHead(ctx, c.Repo, branch)
	if err != nil {
		return nil, err
	}
	for _, pr := range prs {
		if pr.Open {
			return blocked("pull request already open",
				fmt.Sprintf("**Blocked: pull request #%d is already open from `%s`.** Close it to let foreman open its own.",
					pr.Number, branch)), nil
		}
	}
	// Overwrites whatever a run interrupted before opening the PR left.
	if t, err := e.retried(ctx, c, "push the branch", func() error {
		return e.Workspace.Push(ctx, c.Repo, dir, tip, branch, true)
	}); t != nil || err != nil {
		return t, err
	}
	return &Transition{
		To:     store.InReview,
		Reason: "pull request opened",
		Comment: fmt.Sprintf("**In review.** The checks passed and foreman pushed `%s`. "+
			"It opened a pull request that refers to this issue; a pre-review follows.", branch),
		OpenPR: &store.PullRequest{Head: branch, Base: dirs.BaseBranch, Title: prTitle(snap), Body: prBody(c, snap, p, res)},
		Update: func(is *store.Issue) {
			is.BaseBranch, is.BaseSHA, is.DevSHA, is.HeadSHA = dirs.BaseBranch, dirs.Base, tip, tip
			is.PRNumber, is.RevisionRounds, is.LastReviewID, is.PrereviewDone = 0, 0, 0, false
			is.MergeSHA = ""
		},
	}, nil
}

// develop runs the agent, once more if its output is invalid, and turns
// a failure into a blocking transition. what names the run in comments.
func (e *Engine) develop(ctx context.Context, c *Case, req develop.Request, what string) (develop.Result, *Transition, error) {
	res, err := e.Developer.Develop(ctx, req)
	var inv *qualify.InvalidOutputError
	if errors.As(err, &inv) {
		first := inv.Raw
		e.Log.Warn("invalid agent output, retrying", "repo", c.Repo.String(), "issue", c.Issue.Number, "run", what, "err", inv.Err)
		res, err = e.Developer.Develop(ctx, req)
		if errors.As(err, &inv) {
			path, serr := e.saveRaw(c, first, inv.Raw)
			if serr != nil {
				return develop.Result{}, nil, serr
			}
			e.Log.Error("invalid agent output twice", "repo", c.Repo.String(), "issue", c.Issue.Number, "run", what, "saved", path)
			return develop.Result{}, blocked("invalid agent output: "+inv.Err.Error(), fmt.Sprintf(
				"**Blocked: %s failed.** The agent's output was invalid twice. "+
					"A maintainer can find it in the foreman data directory.", what)), nil
		}
	}
	if err != nil {
		if ctx.Err() != nil {
			return develop.Result{}, nil, err // shutting down: start again after restart
		}
		e.Log.Error("agent failed", "repo", c.Repo.String(), "issue", c.Issue.Number, "run", what, "err", err)
		return develop.Result{}, blocked("agent failure: "+err.Error(), fmt.Sprintf(
			"**Blocked: %s failed.** The agent did not complete. A maintainer can find details in the foreman logs.", what)), nil
	}
	if res.Status == develop.Failed {
		return develop.Result{}, blocked("agent reported failure", fmt.Sprintf(
			"**Blocked: %s failed.** The agent stopped and reported:\n\n%s", what,
			qualify.Markdown(res.FailureReport, maxSection))), nil
	}
	return res, nil, nil
}

// verify copies the agent's branch into a clone foreman owns and runs
// every deterministic check there. A failed check blocks the issue with
// the check's name.
func (e *Engine) verify(ctx context.Context, c *Case, workdir, branch string, p project.Config, r develop.Range) (
	dir, tip string, changes []develop.TestChange, t *Transition, err error,
) {
	dir, tip, err = e.Workspace.Snapshot(ctx, c.Repo, c.Issue.Number, workdir, branch)
	if err != nil {
		if ctx.Err() != nil {
			return "", "", nil, nil, err
		}
		return "", "", nil, blocked("branch unreadable: "+err.Error(), fmt.Sprintf(
			"**Blocked: foreman could not read the branch `%s` after the agent's run.**\n\n%s",
			branch, qualify.Fenced(err.Error(), maxDetail))), nil
	}
	r.Tip = tip
	changes, err = develop.CheckHistory(ctx, dir, p, r)
	if err == nil {
		err = develop.RunCommands(ctx, dir, p)
	}
	var ce *develop.CheckError
	switch {
	case ctx.Err() != nil:
		return "", "", nil, nil, ctx.Err()
	case errors.As(err, &ce):
		return "", "", nil, blocked("check failed: "+ce.Check, fmt.Sprintf(
			"**Blocked: check `%s` failed.** foreman checks the agent's branch itself before pushing it.\n\n%s",
			ce.Check, qualify.Fenced(ce.Detail, maxDetail))), nil
	case err != nil:
		return "", "", nil, nil, err
	}
	return dir, tip, changes, nil, nil
}

// prTitle is the approved issue title, on one line, as plain text.
func prTitle(s qualify.Snapshot) string {
	t := strings.Join(strings.Fields(qualify.MakeVisible(s.Title)), " ")
	if len(t) > 200 {
		t = strings.ToValidUTF8(t[:200], "") + "..."
	}
	return t
}

// prBody is the plan, the review guide, the agent's notes and a reference
// to the issue. "Refs", never "Closes": the issue stays open until
// acceptance.
func prBody(c *Case, s qualify.Snapshot, p project.Config, res develop.Result) string {
	var b strings.Builder
	b.WriteString(qualify.Markdown(res.Plan, maxSection))
	b.WriteString("\n\n")
	b.WriteString(qualify.Markdown(res.ReviewGuide, maxSection))
	b.WriteString(agentNotes(res))
	fmt.Fprintf(&b, "\n\n---\n\nRefs #%d\n\n", s.Number)
	fmt.Fprintf(&b, "_Developed by foreman from the approved snapshot of #%d (sha256 `%s`). "+
		"foreman checked the branch itself: tests committed first, no protected path, at most %d changed lines, "+
		"`%s` and `%s` pass._", s.Number, c.Issue.SnapshotSHA, p.MaxDiffLines, p.Test, p.Lint)
	return b.String()
}

// agentNotes renders the text the agent wrote before its JSON object as
// a quoted, untrusted block: none of its markup is rendered.
func agentNotes(res develop.Result) string {
	if strings.TrimSpace(res.Notes) == "" {
		return ""
	}
	return "\n\n## Agent notes\n\n_Written by the agent, shown as plain text._\n\n" + qualify.Fenced(res.Notes, maxNotes)
}

// inReview follows the pull request: merged, closed, pre-review, and
// revisions after an authorized user requests changes.
func (e *Engine) inReview(ctx context.Context, c *Case) (*Transition, error) {
	if c.Issue.PRNumber == 0 {
		return nil, nil // the PR is not open yet: the outbox retries
	}
	pr, err := e.GitHub.PullRequest(ctx, c.Repo, c.Issue.PRNumber)
	if err != nil {
		return nil, err
	}
	switch {
	case pr.Merged:
		return &Transition{
			To:     store.Merged,
			Reason: "pull request merged",
			Comment: fmt.Sprintf("**Merged.** Pull request #%d was merged. "+
				"This issue stays open until the change is accepted.", pr.Number),
			Update: func(is *store.Issue) { is.MergeSHA = pr.MergeCommitSHA },
		}, nil
	case !pr.Open:
		return blocked("pull request closed without merge",
			fmt.Sprintf("**Blocked: pull request #%d was closed without being merged.** A human needs to decide what happens next.",
				pr.Number)), nil
	case !c.Issue.PrereviewDone:
		return e.preReview(ctx, c, pr)
	}
	return e.review(ctx, c, pr)
}

// branchWorkspace prepares a clone of the pushed branch, deep enough to
// reach the commit the work started from.
func (e *Engine) branchWorkspace(ctx context.Context, c *Case, pr github.PullRequest) (workspace.Dirs, *Transition, error) {
	var dirs workspace.Dirs
	t, err := e.prepare(ctx, c, func() (err error) {
		dirs, err = e.Workspace.PrepareBranch(ctx, c.Repo, c.Issue.Number, Branch(c.Issue), pr.Commits+1, c.Issue.BaseSHA)
		return err
	})
	return dirs, t, err
}

// preReview runs a separate, read-only agent on the PR diff and posts
// its findings on the PR. A high finding, a dependency or auth change, or
// a pre-review that could not run labels the PR high-risk.
func (e *Engine) preReview(ctx context.Context, c *Case, pr github.PullRequest) (*Transition, error) {
	snap, q, err := approved(c.Issue)
	if err != nil {
		return nil, err
	}
	dirs, t, err := e.branchWorkspace(ctx, c, pr)
	if t != nil || err != nil {
		return t, err
	}
	diff, err := workspace.Git(ctx, dirs.Workdir, "diff", "--no-renames", c.Issue.BaseSHA, "HEAD")
	if err != nil {
		return nil, err
	}
	if len(diff) > maxDiff {
		diff = strings.ToValidUTF8(diff[:maxDiff], "") + "\n[... truncated]"
	}
	files, err := develop.ChangedFiles(ctx, dirs.Workdir, c.Issue.BaseSHA, "HEAD")
	if err != nil {
		return nil, err
	}
	commits, err := develop.Commits(ctx, dirs.Workdir, c.Issue.BaseSHA, "HEAD")
	if err != nil {
		return nil, err
	}
	req := develop.PreReviewRequest{
		Workdir: dirs.Workdir, SkillsDir: dirs.SkillsDir, Issue: snap, AcceptanceCriteria: q.AcceptanceCriteria, Diff: diff,
		PRBody: pr.Body, Commits: commits,
	}
	res, err := e.Developer.PreReview(ctx, req)
	var inv *qualify.InvalidOutputError
	if errors.As(err, &inv) {
		e.Log.Warn("invalid pre-review output, retrying", "repo", c.Repo.String(), "issue", c.Issue.Number, "err", inv.Err)
		res, err = e.Developer.PreReview(ctx, req)
	}
	if err != nil && ctx.Err() != nil {
		return nil, err
	}

	var b strings.Builder
	high := false
	if err != nil {
		e.Log.Error("pre-review failed", "repo", c.Repo.String(), "issue", c.Issue.Number, "err", err)
		b.WriteString("## Pre-review\n\nThe pre-review could not run, so foreman labels this pull request high-risk: " +
			"review it with extra care.")
		high = true
	} else {
		b.WriteString(qualify.Markdown(res.Report, maxSection))
		high = res.High()
	}
	if risks := develop.Risky(files); len(risks) > 0 {
		b.WriteString("\n\n### Deterministic checks\n\nThis pull request touches sensitive files:\n")
		for _, r := range risks {
			b.WriteString("- " + r + "\n")
		}
		high = true
	}
	t = &Transition{
		To:      store.InReview,
		Reason:  "pre-review",
		Comment: b.String(),
		PR:      pr.Number,
		Update:  func(is *store.Issue) { is.PrereviewDone = true },
	}
	if high {
		t.AddLabels = []string{LabelHighRisk}
		t.Comment += fmt.Sprintf("\n\nLabeled `%s`.", LabelHighRisk)
	}
	return t, nil
}

// review answers the newest changes request of an authorized user that
// was not handled yet with one revision, up to MaxRevisions. Reviews by
// anyone else are ignored.
func (e *Engine) review(ctx context.Context, c *Case, pr github.PullRequest) (*Transition, error) {
	reviews, err := e.GitHub.Reviews(ctx, c.Repo, pr.Number)
	if err != nil {
		return nil, err
	}
	var rv *github.Review
	for i, r := range reviews {
		if r.ID <= c.Issue.LastReviewID || r.State != "CHANGES_REQUESTED" || e.isBot(r.Author) {
			continue
		}
		if !e.Config.IsAuthorized(r.Author) {
			e.Log.Debug("review by an unauthorized user ignored", "repo", c.Repo.String(), "pr", pr.Number, "author", r.Author)
			continue
		}
		rv = &reviews[i]
	}
	if rv == nil {
		return nil, nil
	}
	if c.Issue.RevisionRounds >= MaxRevisions {
		return &Transition{
			To:     store.Blocked,
			Reason: reasonRevisionLimit,
			Comment: fmt.Sprintf("**Blocked: changes were requested again after %d revisions** (review by @%s on #%d). "+
				"A human needs to take over.", MaxRevisions, rv.Author, pr.Number),
			Update: func(is *store.Issue) { is.LastReviewID = rv.ID },
		}, nil
	}
	return e.revise(ctx, c, pr, *rv)
}

// revise runs one revision on the pushed branch, checks it like the
// development run and pushes it as a fast-forward.
func (e *Engine) revise(ctx context.Context, c *Case, pr github.PullRequest, rv github.Review) (*Transition, error) {
	snap, q, err := approved(c.Issue)
	if err != nil {
		return nil, err
	}
	comments, err := e.GitHub.ReviewComments(ctx, c.Repo, pr.Number, rv.ID)
	if err != nil {
		return nil, err
	}
	dirs, t, err := e.branchWorkspace(ctx, c, pr)
	if t != nil || err != nil {
		return t, err
	}
	p, t := projectConfig(dirs)
	if t != nil {
		return t, nil
	}
	round := c.Issue.RevisionRounds + 1
	what := fmt.Sprintf("revision %d of %d", round, MaxRevisions)
	review := &develop.Review{Author: rv.Author, Body: rv.Body}
	for _, cm := range comments {
		review.Comments = append(review.Comments, develop.ReviewComment{Path: cm.Path, Line: cm.Line, Body: cm.Body})
	}
	branch := Branch(c.Issue)
	req := develop.Request{
		Workdir: dirs.Workdir, SkillsDir: dirs.SkillsDir, Branch: branch, Project: p,
		Issue: snap, Summary: q.Summary, AcceptanceCriteria: q.AcceptanceCriteria, Review: review,
	}
	res, t, err := e.develop(ctx, c, req, what)
	if t != nil || err != nil {
		return t, err
	}
	head := c.Issue.HeadSHA
	dir, tip, changes, t, err := e.verify(ctx, c, dirs.Workdir, branch, p,
		develop.Range{Base: c.Issue.BaseSHA, DevTip: c.Issue.DevSHA, Head: head})
	if t != nil || err != nil {
		return t, err
	}
	if tip == head {
		return blocked("revision without commit", fmt.Sprintf(
			"**Blocked: %s made no commit** in answer to the review by @%s on #%d.", what, rv.Author, pr.Number)), nil
	}
	if t, err := e.retried(ctx, c, "push the branch", func() error {
		return e.Workspace.Push(ctx, c.Repo, dir, tip, branch, false)
	}); t != nil || err != nil {
		return t, err
	}
	files, err := develop.ChangedFiles(ctx, dir, head, tip)
	if err != nil {
		return nil, err
	}
	t = &Transition{
		To:      store.InReview,
		Reason:  fmt.Sprintf("revision %d for review %d", round, rv.ID),
		Comment: revisionComment(what, rv, res, changes, develop.Risky(files)),
		PR:      pr.Number,
		Update: func(is *store.Issue) {
			is.RevisionRounds, is.LastReviewID, is.HeadSHA = round, rv.ID, tip
		},
	}
	if len(develop.Risky(files)) > 0 {
		t.AddLabels = []string{LabelHighRisk}
	}
	return t, nil
}

func revisionComment(what string, rv github.Review, res develop.Result, changes []develop.TestChange, risks []string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "## foreman: %s\n\nThis answers the changes requested by @%s. The checks passed again.\n\n", what, rv.Author)
	b.WriteString(qualify.Markdown(res.Plan, maxSection))
	if len(changes) > 0 {
		b.WriteString("\n\n### Tests changed in this revision:\n\n")
		for _, ch := range changes {
			fmt.Fprintf(&b, "- %s (%s)\n", develop.Code(ch.Path), ch.Status)
		}
	}
	if strings.TrimSpace(res.ReviewGuide) != "" {
		b.WriteString("\n\n")
		b.WriteString(qualify.Markdown(res.ReviewGuide, maxSection))
	}
	b.WriteString(agentNotes(res))
	if len(risks) > 0 {
		fmt.Fprintf(&b, "\n\n### Deterministic checks\n\nThis revision touches sensitive files, labeled `%s`:\n", LabelHighRisk)
		for _, r := range risks {
			b.WriteString("- " + r + "\n")
		}
	}
	return b.String()
}
