package orchestrator_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/Bibzor/foreman/internal/config"
	"github.com/Bibzor/foreman/internal/develop"
	"github.com/Bibzor/foreman/internal/github"
	"github.com/Bibzor/foreman/internal/qualify"
	"github.com/Bibzor/foreman/internal/sandbox"
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

	prs          map[string]*fakePR // "owner/name#n"
	nextPR       int
	failCreatePR int // CreatePullRequest creates the PR, then fails, this many times
}

type fakePR struct {
	github.PullRequest
	base, title string
	reviews     []github.Review
	comments    map[int64][]github.ReviewComment
}

type fakeIssue struct {
	github.Issue
	comments    []github.Comment
	events      []github.LabelEvent
	closeReason string // the state_reason foreman closed it with
}

func newFakeGitHub(now func() time.Time) *fakeGitHub {
	return &fakeGitHub{now: now, issues: map[string]*fakeIssue{}, labels: map[string][]string{}, nextID: 1000,
		prs: map[string]*fakePR{}, nextPR: 100}
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

func (f *fakeGitHub) label(name, actor string) { f.labelOn(n, name, actor) }

func (f *fakeGitHub) labelOn(num int, name, actor string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	is := f.issues[key(repo, num)]
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

func (f *fakeGitHub) botComments() []string { return f.botCommentsOn(n) }

func (f *fakeGitHub) botCommentsOn(num int) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, c := range f.issues[key(repo, num)].comments {
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

// pr returns the only pull request, or nil.
func (f *fakeGitHub) pr() *fakePR {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.prs) != 1 {
		return nil
	}
	for _, p := range f.prs {
		cp := *p
		return &cp
	}
	return nil
}

func (f *fakeGitHub) prLabels(num int) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.issues[key(repo, num)].Labels)
}

func (f *fakeGitHub) closePR(num int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.prs[key(repo, num)].Open = false
}

func (f *fakeGitHub) review(num int, author, state, body string, comments ...github.ReviewComment) {
	f.reviewAt(num, "", author, state, body, comments...)
}

// reviewAt is a review made on commit sha of the PR.
func (f *fakeGitHub) reviewAt(num int, sha, author, state, body string, comments ...github.ReviewComment) {
	f.mu.Lock()
	defer f.mu.Unlock()
	p := f.prs[key(repo, num)]
	id := f.id()
	p.reviews = append(p.reviews, github.Review{ID: id, Author: author, State: state, Body: body, SubmittedAt: f.now(), CommitID: sha})
	p.comments[id] = comments
}

// mergeAt records the PR as merged, with its final head and merge
// commit.
func (f *fakeGitHub) mergeAt(num int, head, merge string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	p := f.prs[key(repo, num)]
	p.Open, p.Merged, p.HeadSHA, p.MergeCommitSHA = false, true, head, merge
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

func (f *fakeGitHub) CloseIssue(_ context.Context, repo config.Repo, n int, reason string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	is, err := f.get(repo, n)
	if err != nil {
		return err
	}
	is.Open, is.closeReason = false, reason
	return nil
}

func (f *fakeGitHub) closeReason() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.issues[key(repo, n)].closeReason
}

func (f *fakeGitHub) CreatePullRequest(_ context.Context, repo config.Repo, head, base, title, body string) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for k, p := range f.prs {
		if strings.HasPrefix(k, repo.String()+"#") && p.Open && p.Head == head {
			return 0, &github.APIError{Status: 422, Message: "A pull request already exists for " + head}
		}
	}
	f.nextPR++
	num := f.nextPR
	f.prs[key(repo, num)] = &fakePR{
		PullRequest: github.PullRequest{Number: num, Open: true, Head: head, Body: body, Commits: 50},
		base:        base, title: title, comments: map[int64][]github.ReviewComment{},
	}
	f.issues[key(repo, num)] = &fakeIssue{Issue: github.Issue{Number: num, Title: title, Body: body, Author: bot, Open: true}}
	if f.failCreatePR > 0 {
		f.failCreatePR--
		return 0, errors.New("connection reset after the pull request was created")
	}
	return num, nil
}

func (f *fakeGitHub) PullRequestsByHead(_ context.Context, repo config.Repo, branch string) ([]github.PullRequest, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []github.PullRequest
	for k, p := range f.prs {
		if strings.HasPrefix(k, repo.String()+"#") && p.Head == branch {
			out = append(out, p.PullRequest)
		}
	}
	return out, nil
}

func (f *fakeGitHub) PullRequest(_ context.Context, repo config.Repo, num int) (github.PullRequest, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	p, ok := f.prs[key(repo, num)]
	if !ok {
		return github.PullRequest{}, &github.APIError{Status: 404, Message: "Not Found"}
	}
	return p.PullRequest, nil
}

func (f *fakeGitHub) Reviews(_ context.Context, repo config.Repo, num int) ([]github.Review, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.prs[key(repo, num)].reviews), nil
}

func (f *fakeGitHub) ReviewComments(_ context.Context, repo config.Repo, num int, id int64) ([]github.ReviewComment, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.prs[key(repo, num)].comments[id]), nil
}

// fakeAgent returns scripted answers in order.
type fakeAgent struct {
	mu       sync.Mutex
	answers  []answer
	requests []qualify.Request

	devAnswers []devAnswer
	devReqs    []develop.Request
	preAnswers []preAnswer
	preReqs    []develop.PreReviewRequest
}

// devAnswer is a scripted development run: work runs in the workdir, as
// the agent would, then res and err are returned.
type devAnswer struct {
	work func(dir string)
	res  develop.Result
	err  error
}

type preAnswer struct {
	res develop.PreReviewResult
	err error
}

func (a *fakeAgent) pushDev(ans ...devAnswer) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.devAnswers = append(a.devAnswers, ans...)
}

func (a *fakeAgent) pushPre(ans ...preAnswer) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.preAnswers = append(a.preAnswers, ans...)
}

func (a *fakeAgent) Develop(_ context.Context, req develop.Request) (develop.Result, error) {
	a.mu.Lock()
	a.devReqs = append(a.devReqs, req)
	if len(a.devAnswers) == 0 {
		a.mu.Unlock()
		return develop.Result{}, errors.New("fake agent: no scripted development")
	}
	ans := a.devAnswers[0]
	a.devAnswers = a.devAnswers[1:]
	a.mu.Unlock()
	if ans.work != nil {
		ans.work(req.Workdir)
	}
	return ans.res, ans.err
}

func (a *fakeAgent) PreReview(_ context.Context, req develop.PreReviewRequest) (develop.PreReviewResult, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.preReqs = append(a.preReqs, req)
	if len(a.preAnswers) == 0 {
		return develop.PreReviewResult{}, errors.New("fake agent: no scripted pre-review")
	}
	ans := a.preAnswers[0]
	a.preAnswers = a.preAnswers[1:]
	return ans.res, ans.err
}

func (a *fakeAgent) devCalls() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.devReqs)
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

type fakeWorkspace struct {
	dir  string
	fail int // Prepare fails this many times
}

func (w *fakeWorkspace) Prepare(_ context.Context, repo config.Repo, n int) (workspace.Dirs, error) {
	if w.fail > 0 {
		w.fail--
		return workspace.Dirs{}, fmt.Errorf("git clone: exit status 128: could not resolve host (left %d)", w.fail)
	}
	return workspace.Dirs{Workdir: fmt.Sprintf("%s/work/%s/%d", w.dir, repo, n), SkillsDir: w.dir + "/skills"}, nil
}

var errNoGit = errors.New("fake workspace: no git")

func (w *fakeWorkspace) PrepareDev(context.Context, config.Repo, int, string) (workspace.Dirs, error) {
	return workspace.Dirs{}, errNoGit
}

func (w *fakeWorkspace) PrepareBranch(context.Context, config.Repo, int, string, int, string) (workspace.Dirs, error) {
	return workspace.Dirs{}, errNoGit
}

func (w *fakeWorkspace) PrepareAt(context.Context, config.Repo, int, string) (workspace.Dirs, error) {
	return workspace.Dirs{}, errNoGit
}

// fakeDocker is a docker Runner that records every call. fail maps a
// subcommand to the output it fails with; down makes every call fail, as
// when the daemon is unreachable.
type fakeDocker struct {
	mu    sync.Mutex
	calls [][]string
	ps    string
	fail  map[string]string
	down  int // the next down calls fail
}

func (d *fakeDocker) Run(_ context.Context, args ...string) (string, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.calls = append(d.calls, slices.Clone(args))
	if d.down > 0 {
		d.down--
		return "Cannot connect to the Docker daemon", errors.New("exit status 1")
	}
	if out, ok := d.fail[args[0]]; ok {
		return out, errors.New("exit status 1")
	}
	if args[0] == "ps" {
		return d.ps, nil
	}
	return "", nil
}

// calledWith returns the calls of a docker subcommand.
func (d *fakeDocker) calledWith(sub string) [][]string {
	d.mu.Lock()
	defer d.mu.Unlock()
	var out [][]string
	for _, c := range d.calls {
		if c[0] == sub {
			out = append(out, c)
		}
	}
	return out
}

// subcommands lists the docker subcommands called, in order.
func (d *fakeDocker) subcommands() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	var out []string
	for _, c := range d.calls {
		out = append(out, c[0])
	}
	return out
}

// fakeProbe answers every smoke check with status, or with err.
type fakeProbe struct {
	mu     sync.Mutex
	status int
	err    error
	urls   []string
}

func (p *fakeProbe) probe(_ context.Context, url string) (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.urls = append(p.urls, url)
	return p.status, p.err
}

func (w *fakeWorkspace) Snapshot(context.Context, config.Repo, int, string, string) (string, string, error) {
	return "", "", errNoGit
}

func (w *fakeWorkspace) Checkout(context.Context, config.Repo, int, string, string) (string, error) {
	return "", errNoGit
}

// fakeSandbox stands for the Docker sandbox. Exec runs the command on the
// host, in the spec's workspace, so `true` and `false` mean what they say;
// it records every call as "setup <workspace>", "exec <name> <workspace>"
// and "cleanup".
type fakeSandbox struct {
	mu        sync.Mutex
	events    []string
	specs     []sandbox.Spec
	failSetup int // the next failSetup setups fail
}

func (s *fakeSandbox) record(e string, spec sandbox.Spec) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events = append(s.events, e)
	s.specs = append(s.specs, spec)
}

func (s *fakeSandbox) Setup(_ context.Context, spec sandbox.Spec, out io.Writer) error {
	if err := spec.Validate(); err != nil {
		return err
	}
	s.record("setup "+spec.Workspace, spec)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failSetup > 0 {
		s.failSetup--
		_, _ = io.WriteString(out, "go: downloading example.com/x: connection refused")
		return &sandbox.ExitError{Code: 1}
	}
	return nil
}

func (s *fakeSandbox) Exec(ctx context.Context, spec sandbox.Spec, name string, out io.Writer) error {
	if err := spec.Validate(); err != nil {
		return err
	}
	s.record("exec "+name+" "+spec.Workspace, spec)
	c, ok := spec.Command(name)
	if !ok {
		return fmt.Errorf("no command %s", name)
	}
	cmd := exec.CommandContext(ctx, c.Argv[0], c.Argv[1:]...)
	cmd.Dir = spec.Workspace
	cmd.Stdout, cmd.Stderr = out, out
	var ee *exec.ExitError
	if err := cmd.Run(); errors.As(err, &ee) {
		return &sandbox.ExitError{Code: ee.ExitCode()}
	} else if err != nil {
		return &sandbox.ExitError{Code: 127}
	}
	return nil
}

func (s *fakeSandbox) Cleanup(context.Context, config.Repo, int) error {
	s.record("cleanup", sandbox.Spec{})
	return nil
}

// kinds lists the events without their workspace.
func (s *fakeSandbox) kinds() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for _, e := range s.events {
		f := strings.Fields(e)
		if f[0] == "exec" {
			out = append(out, f[0]+" "+f[1])
		} else {
			out = append(out, f[0])
		}
	}
	return out
}

func (w *fakeWorkspace) Push(context.Context, config.Repo, string, string, string, bool) error {
	return errNoGit
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
