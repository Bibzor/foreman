package orchestrator_test

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/Bibzor/foreman/internal/config"
	"github.com/Bibzor/foreman/internal/github"
	"github.com/Bibzor/foreman/internal/qualify"
	"github.com/Bibzor/foreman/internal/workspace"
)

const bot = "foreman-app[bot]"

// fakeGitHub is an in-memory GitHub for one or more repositories.
type fakeGitHub struct {
	mu     sync.Mutex
	now    func() time.Time
	nextID int64
	issues map[string]*fakeIssue // "owner/name#n"
	labels map[string][]string   // repo -> created labels

	failComments int // CreateComment fails this many times (after posting when postThenFail)
	postThenFail bool
	created      int // comments created by the bot
}

type fakeIssue struct {
	github.Issue
	comments []github.Comment
	events   []github.LabelEvent
}

func newFakeGitHub(now func() time.Time) *fakeGitHub {
	return &fakeGitHub{now: now, issues: map[string]*fakeIssue{}, labels: map[string][]string{}, nextID: 1000}
}

func key(repo config.Repo, n int) string { return fmt.Sprintf("%s#%d", repo, n) }

func (f *fakeGitHub) id() int64 { f.nextID++; return f.nextID }

func (f *fakeGitHub) get(repo config.Repo, n int) (*fakeIssue, error) {
	is, ok := f.issues[key(repo, n)]
	if !ok {
		return nil, &github.APIError{Status: 404, Message: "Not Found"}
	}
	return is, nil
}

// Test helpers acting as humans.

func (f *fakeGitHub) open(repo config.Repo, n int, author, title, body string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.issues[key(repo, n)] = &fakeIssue{Issue: github.Issue{Number: n, Title: title, Body: body, Author: author, Open: true}}
}

func (f *fakeGitHub) label(name, actor string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	is := f.issues[key(repo, n)]
	if !is.HasLabel(name) {
		is.Labels = append(is.Labels, name)
	}
	is.events = append(is.events, github.LabelEvent{ID: f.id(), Label: name, Actor: actor, CreatedAt: f.now()})
}

func (f *fakeGitHub) comment(author, body string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	is := f.issues[key(repo, n)]
	is.comments = append(is.comments, github.Comment{ID: f.id(), Author: author, Body: body, CreatedAt: f.now()})
}

func (f *fakeGitHub) edit(body string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.issues[key(repo, n)].Body = body
}

func (f *fakeGitHub) closeByHuman() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.issues[key(repo, n)].Open = false
}

func (f *fakeGitHub) botComments() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, c := range f.issues[key(repo, n)].comments {
		if c.Author == bot {
			out = append(out, c.Body)
		}
	}
	return out
}

func (f *fakeGitHub) issue() github.Issue {
	f.mu.Lock()
	defer f.mu.Unlock()
	is := f.issues[key(repo, n)].Issue
	is.Labels = slices.Clone(is.Labels)
	return is
}

// GitHub interface.

func (f *fakeGitHub) BotLogin(context.Context) (string, error) { return bot, nil }

func (f *fakeGitHub) EnsureLabels(_ context.Context, repo config.Repo, labels []github.Label) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, l := range labels {
		if !slices.Contains(f.labels[repo.String()], l.Name) {
			f.labels[repo.String()] = append(f.labels[repo.String()], l.Name)
		}
	}
	return nil
}

func (f *fakeGitHub) OpenIssuesWithLabel(_ context.Context, repo config.Repo, label string) ([]github.Issue, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []github.Issue
	for k, is := range f.issues {
		if strings.HasPrefix(k, repo.String()+"#") && is.Open && is.HasLabel(label) {
			out = append(out, is.Issue)
		}
	}
	slices.SortFunc(out, func(a, b github.Issue) int { return a.Number - b.Number })
	return out, nil
}

func (f *fakeGitHub) Issue(_ context.Context, repo config.Repo, n int) (github.Issue, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	is, err := f.get(repo, n)
	if err != nil {
		return github.Issue{}, err
	}
	out := is.Issue
	out.Labels = slices.Clone(is.Labels)
	return out, nil
}

func (f *fakeGitHub) Comments(_ context.Context, repo config.Repo, n int) ([]github.Comment, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	is, err := f.get(repo, n)
	if err != nil {
		return nil, err
	}
	return slices.Clone(is.comments), nil
}

func (f *fakeGitHub) LabelEvents(_ context.Context, repo config.Repo, n int) ([]github.LabelEvent, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	is, err := f.get(repo, n)
	if err != nil {
		return nil, err
	}
	return slices.Clone(is.events), nil
}

func (f *fakeGitHub) AddLabel(_ context.Context, repo config.Repo, n int, label string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	is, err := f.get(repo, n)
	if err != nil {
		return err
	}
	if !is.HasLabel(label) {
		is.Labels = append(is.Labels, label)
		is.events = append(is.events, github.LabelEvent{ID: f.id(), Label: label, Actor: bot, CreatedAt: f.now()})
	}
	return nil
}

func (f *fakeGitHub) RemoveLabel(_ context.Context, repo config.Repo, n int, label string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	is, err := f.get(repo, n)
	if err != nil {
		return err
	}
	is.Labels = slices.DeleteFunc(is.Labels, func(l string) bool { return strings.EqualFold(l, label) })
	return nil
}

func (f *fakeGitHub) CreateComment(_ context.Context, repo config.Repo, n int, body string) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	is, err := f.get(repo, n)
	if err != nil {
		return 0, err
	}
	if f.failComments > 0 && !f.postThenFail {
		f.failComments--
		return 0, errors.New("github is down")
	}
	id := f.id()
	is.comments = append(is.comments, github.Comment{ID: id, Author: bot, Body: body, CreatedAt: f.now()})
	f.created++
	if f.failComments > 0 {
		f.failComments--
		return 0, errors.New("connection reset after the comment was created")
	}
	return id, nil
}

func (f *fakeGitHub) CloseIssue(_ context.Context, repo config.Repo, n int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	is, err := f.get(repo, n)
	if err != nil {
		return err
	}
	is.Open = false
	return nil
}

// fakeAgent returns scripted answers in order.
type fakeAgent struct {
	mu       sync.Mutex
	answers  []answer
	requests []qualify.Request
}

type answer struct {
	res    qualify.Result
	err    error
	during func() // runs while the agent "works"
}

func (a *fakeAgent) push(ans ...answer) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.answers = append(a.answers, ans...)
}

func (a *fakeAgent) Qualify(_ context.Context, req qualify.Request) (qualify.Result, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.requests = append(a.requests, req)
	if len(a.answers) == 0 {
		return qualify.Result{}, errors.New("fake agent: no scripted answer")
	}
	ans := a.answers[0]
	a.answers = a.answers[1:]
	if ans.during != nil {
		ans.during()
	}
	return ans.res, ans.err
}

func (a *fakeAgent) calls() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.requests)
}

type fakeWorkspace struct{ dir string }

func (w fakeWorkspace) Prepare(_ context.Context, repo config.Repo, n int) (workspace.Dirs, error) {
	return workspace.Dirs{Workdir: fmt.Sprintf("%s/work/%s/%d", w.dir, repo, n), SkillsDir: w.dir + "/skills"}, nil
}

// Scripted agent answers.

func ready() answer {
	return answer{res: qualify.Result{
		Decision: qualify.Ready, Summary: "Add GET /version.", Size: "S",
		AcceptanceCriteria: []string{"Given a build, when GET /version, then 200 with the version"},
		Risks:              []string{"none worth noting"},
	}}
}

func needsInfo() answer {
	return answer{res: qualify.Result{Decision: qualify.NeedsInfo, Summary: "Unclear.", Size: "S", Questions: []string{"Which format?"}}}
}

func outOfScope() answer {
	return answer{res: qualify.Result{Decision: qualify.OutOfScope, Summary: "Auth work.", Size: "M", OutOfScopeReason: "AGENTS.md excludes authentication"}}
}

func agentSaysSuspicious() answer {
	a := ready()
	a.res.Suspicious, a.res.SuspiciousReasons = true, []string{"asks to change CI"}
	return a
}

func invalid(raw string) answer {
	return answer{err: &qualify.InvalidOutputError{Raw: raw, Err: errors.New("missing \"size\"")}}
}

func crash() answer { return answer{err: errors.New("agent run failed: exit status 1")} }
