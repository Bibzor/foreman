// Package develop holds everything about developing an issue that does not
// depend on a particular agent: the Agent interface, the prompts, the
// validation of the agent's output, and the checks foreman runs itself on
// the result without trusting the agent.
package develop

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/Bibzor/foreman/internal/project"
	"github.com/Bibzor/foreman/internal/qualify"
)

// Statuses of a development run.
const (
	Done   = "done"
	Failed = "failed"
)

// Result is the output of a development or revision run.
type Result struct {
	Status        string `json:"status"`
	Plan          string `json:"plan"`
	ReviewGuide   string `json:"review_guide"`
	FailureReport string `json:"failure_report"`
	// Notes is the free text the agent wrote before the JSON object. It
	// is untrusted.
	Notes string `json:"-"`
}

// Review is the review a revision answers. It is untrusted.
type Review struct {
	Author   string
	Body     string
	Comments []ReviewComment
}

// ReviewComment is an inline comment of a Review.
type ReviewComment struct {
	Path string
	Line int
	Body string
}

// Request is one development or revision run.
type Request struct {
	// Workdir is the clone, with Branch checked out and no credential.
	Workdir string
	// SkillsDir holds the verified skills, outside the clone.
	SkillsDir string
	Branch    string
	Project   project.Config
	// Issue is the snapshot that was approved, never the live issue.
	Issue qualify.Snapshot
	// Summary and AcceptanceCriteria come from the approved
	// qualification.
	Summary            string
	AcceptanceCriteria []string
	// Review is nil for development, the review to address for a
	// revision.
	Review *Review
	// Feedback is set when a previous implementation was merged and
	// rejected at acceptance: this development addresses it.
	Feedback *Feedback
	// SandboxSpec is the spec of the agent's sandbox, outside the clone.
	SandboxSpec string
	// Commands are the command lines that run the project commands in the
	// sandbox. The agent implementation fills them: they name its binary.
	Commands []AgentCommand
}

// AgentCommand is a project command as the agent runs it.
type AgentCommand struct {
	Line string // the command line the agent may run
	Runs string // the project command it runs in the sandbox
}

// GitCommands are the git commands the agent may run, as prefixes: no
// pager and no external diff, whatever .git/config says.
var GitCommands = []string{
	"git --no-pager status",
	"git --no-pager diff --no-ext-diff",
	"git --no-pager log --no-ext-diff",
	"git --no-pager add",
	"git --no-pager commit",
}

// Feedback is the comment explaining why a merged change was rejected at
// acceptance. It is untrusted.
type Feedback struct {
	Author string
	Body   string
}

// Severities of a pre-review finding.
const (
	High   = "high"
	Medium = "medium"
	Low    = "low"
)

// Finding is one pre-review finding.
type Finding struct {
	Severity string `json:"severity"`
	Location string `json:"location"`
	Text     string `json:"text"`
}

// PreReviewResult is the output of a pre-review.
type PreReviewResult struct {
	Report   string    `json:"report"`
	Findings []Finding `json:"findings"`
}

// High reports whether any finding is of severity high.
func (r PreReviewResult) High() bool {
	for _, f := range r.Findings {
		if f.Severity == High {
			return true
		}
	}
	return false
}

// PreReviewRequest is one pre-review.
type PreReviewRequest struct {
	Workdir            string // the clone, the PR branch checked out
	SkillsDir          string
	Issue              qualify.Snapshot
	AcceptanceCriteria []string
	Diff               string // the PR diff, written by an agent: untrusted
	// PRBody is the PR description (the plan, with "Files to change") and
	// Commits the commits of the PR with the files each touches, computed
	// by foreman with git. Both are written by an agent: untrusted.
	PRBody  string
	Commits []CommitInfo
}

// Agent develops issues and pre-reviews the result. Implementations
// return a *qualify.InvalidOutputError when the agent ran but its output
// is not valid.
type Agent interface {
	Develop(ctx context.Context, req Request) (Result, error)
	PreReview(ctx context.Context, req PreReviewRequest) (PreReviewResult, error)
}

var resultKeys = []string{"status", "plan", "review_guide", "failure_report"}

// Parse validates the final JSON object of a development or revision run
// and keeps the text before it as Notes.
func Parse(text string) (Result, error) {
	var r Result
	notes, err := qualify.DecodeStrict(text, &r, resultKeys)
	if err != nil {
		return Result{}, err
	}
	r.Notes = notes
	var errs []error
	switch r.Status {
	case Done:
		if strings.TrimSpace(r.Plan) == "" {
			errs = append(errs, errors.New("done without a plan"))
		}
		if strings.TrimSpace(r.ReviewGuide) == "" {
			errs = append(errs, errors.New("done without a review guide"))
		}
	case Failed:
		if strings.TrimSpace(r.FailureReport) == "" {
			errs = append(errs, errors.New("failed without a failure report"))
		}
	default:
		errs = append(errs, fmt.Errorf("status %q", r.Status))
	}
	return r, errors.Join(errs...)
}

// ParsePreReview validates the final text of a pre-review.
func ParsePreReview(text string) (PreReviewResult, error) {
	var r PreReviewResult
	if _, err := qualify.DecodeStrict(text, &r, []string{"report", "findings"}); err != nil {
		return PreReviewResult{}, err
	}
	var errs []error
	if strings.TrimSpace(r.Report) == "" {
		errs = append(errs, errors.New("empty report"))
	}
	for i, f := range r.Findings {
		if f.Severity != High && f.Severity != Medium && f.Severity != Low {
			errs = append(errs, fmt.Errorf("finding %d: severity %q", i, f.Severity))
		}
		if strings.TrimSpace(f.Text) == "" {
			errs = append(errs, fmt.Errorf("finding %d: empty text", i))
		}
	}
	return r, errors.Join(errs...)
}
