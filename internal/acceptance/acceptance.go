// Package acceptance is the contract between the state machine and the
// environment a merged change is deployed to for acceptance. The
// orchestrator only knows the Deployer interface; each backend (Docker
// today, Kubernetes later) implements it in its own package.
//
// Backends build their commands themselves from the validated
// project.Acceptance: the project never supplies a command.
package acceptance

import (
	"context"
	"errors"
	"regexp"
	"strings"

	"github.com/Bibzor/foreman/internal/config"
	"github.com/Bibzor/foreman/internal/project"
)

// Deployer deploys merged changes to the acceptance environment of a
// repository. Each repository has one: a new deployment replaces the
// previous one.
type Deployer interface {
	// Deploy builds spec.Dir, which holds spec.SHA, replaces the
	// repository's previous acceptance deployment and starts the new one.
	Deploy(ctx context.Context, spec Spec) (Deployment, error)
	// Smoke polls each check until it answers its expected status, or
	// fails at the backend's deadline.
	Smoke(ctx context.Context, d Deployment, checks []project.Smoke) error
	// Stop removes the deployment.
	Stop(ctx context.Context, d Deployment) error
}

// Spec is what to deploy.
type Spec struct {
	Repo   config.Repo
	Issue  int
	SHA    string // the merge commit
	Dir    string // a clone foreman owns, at SHA
	Config project.Acceptance
}

// Deployment is a running acceptance deployment.
type Deployment struct {
	ID  string // the backend's name for it
	URL string // where people check the change, e.g. http://localhost:18080
	SHA string
	// Spec is what was deployed; backends use it in Smoke and Stop.
	Spec Spec
}

// ErrUnavailable means the backend cannot be reached: the step is worth
// retrying.
var ErrUnavailable = errors.New("the deployment backend is unavailable")

// StageError is a failed build, run or smoke check: retrying will not
// help until a human looks. Log is tailed and redacted, but may quote
// output of the deployed code: it is untrusted.
type StageError struct {
	Stage string // build, run or smoke
	Log   string
}

func (e *StageError) Error() string { return "acceptance " + e.Stage + " failed" }

// Limits of a log quoted in a comment.
const (
	tailLines = 60
	tailBytes = 3000
)

// Tail keeps the end of a log: at most 60 lines and 3000 bytes.
func Tail(s string) string {
	s = strings.TrimRight(s, "\n")
	lines := strings.Split(s, "\n")
	if len(lines) > tailLines {
		lines = lines[len(lines)-tailLines:]
	}
	s = strings.Join(lines, "\n")
	if len(s) > tailBytes {
		s = strings.ToValidUTF8(s[len(s)-tailBytes:], "")
	}
	return s
}

var secrets = []*regexp.Regexp{
	regexp.MustCompile(`(?s)-----BEGIN [A-Z ]*PRIVATE KEY-----.*?(-----END [A-Z ]*PRIVATE KEY-----|$)`),
	regexp.MustCompile(`(?i)(authorization\s*[:=]\s*)[^\r\n]+`),
	regexp.MustCompile(`\bgh[pousr]_[A-Za-z0-9]{20,}`),
	regexp.MustCompile(`\bgithub_pat_[A-Za-z0-9_]{20,}`),
}

// Redact masks credentials a log may contain: GitHub tokens,
// Authorization header values and private keys.
func Redact(s string) string {
	for i, re := range secrets {
		if i == 1 {
			s = re.ReplaceAllString(s, "${1}[redacted]")
			continue
		}
		s = re.ReplaceAllString(s, "[redacted]")
	}
	return s
}
