// Package qualify holds everything about qualifying an issue that does
// not depend on a particular agent: the Agent interface, the issue
// snapshot the agent works on, deterministic checks for hidden content,
// the prompt, validation of the agent's output and the bot comments.
package qualify

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
)

// Decisions, as in the foreman-qualify skill.
const (
	Ready      = "ready"
	NeedsInfo  = "needs_info"
	OutOfScope = "out_of_scope"
)

// Result is the foreman-qualify output.
type Result struct {
	Decision           string   `json:"decision"`
	Summary            string   `json:"summary"`
	AcceptanceCriteria []string `json:"acceptance_criteria"`
	Questions          []string `json:"questions"`
	OutOfScopeReason   string   `json:"out_of_scope_reason"`
	Size               string   `json:"size"`
	Touches            []string `json:"touches"`
	Risks              []string `json:"risks"`
	Suspicious         bool     `json:"suspicious"`
	SuspiciousReasons  []string `json:"suspicious_reasons"`
}

// Request is one qualification.
type Request struct {
	// Workdir is the clone the agent reads, with no credentials in it.
	Workdir string
	// SkillsDir holds the verified skills, outside the clone.
	SkillsDir string
	Issue     Snapshot
}

// Agent qualifies an issue. Implementations return an *InvalidOutputError
// when the agent ran but its output is not a valid Result.
type Agent interface {
	Qualify(ctx context.Context, req Request) (Result, error)
}

// InvalidOutputError carries the raw agent output that failed validation.
// Raw stays on the local disk: it is never posted on GitHub.
type InvalidOutputError struct {
	Raw string
	Err error
}

func (e *InvalidOutputError) Error() string { return "invalid agent output: " + e.Err.Error() }
func (e *InvalidOutputError) Unwrap() error { return e.Err }

// Snapshot is the content of an issue at qualification time, without the
// bot's own comments. Approval binds to its hash.
type Snapshot struct {
	Repo     string            `json:"repo"`
	Number   int               `json:"number"`
	Title    string            `json:"title"`
	Author   string            `json:"author"`
	Body     string            `json:"body"`
	Comments []SnapshotComment `json:"comments"`
}

// SnapshotComment is one comment of a Snapshot.
type SnapshotComment struct {
	ID     int64  `json:"id"`
	Author string `json:"author"`
	Body   string `json:"body"`
}

// Canonical returns the snapshot as JSON and its sha256. Struct fields
// marshal in a fixed order, so equal content gives equal bytes.
func (s Snapshot) Canonical() (data, sha string) {
	if s.Comments == nil {
		s.Comments = []SnapshotComment{}
	}
	b, _ := json.Marshal(s) // only strings and integers: cannot fail
	sum := sha256.Sum256(b)
	return string(b), hex.EncodeToString(sum[:])
}

// ParseSnapshot decodes a stored snapshot.
func ParseSnapshot(data string) (Snapshot, error) {
	var s Snapshot
	err := json.Unmarshal([]byte(data), &s)
	return s, err
}

var resultKeys = []string{
	"decision", "summary", "acceptance_criteria", "questions", "out_of_scope_reason",
	"size", "touches", "risks", "suspicious", "suspicious_reasons",
}

// Parse extracts the JSON object from the agent's final text and
// validates it against the foreman-qualify output shape.
func Parse(text string) (Result, error) {
	var r Result
	if err := DecodeStrict(text, &r, resultKeys); err != nil {
		return Result{}, err
	}
	return r, r.validate()
}

// DecodeStrict extracts the JSON object from an agent's final text (bare,
// or in a single ```json fence) and decodes it into v: every key of keys
// present and not null, no other key, right types.
func DecodeStrict(text string, v any, keys []string) error {
	body := strings.TrimSpace(text)
	if strings.HasPrefix(body, "```") {
		nl := strings.IndexByte(body, '\n')
		if nl < 0 || !strings.HasSuffix(body, "```") || len(body) < nl+4 {
			return errors.New("unterminated code fence")
		}
		if lang := strings.TrimSpace(body[3:nl]); lang != "" && lang != "json" {
			return fmt.Errorf("code fence language %q", lang)
		}
		body = strings.TrimSpace(body[nl+1 : len(body)-3])
	}

	var got map[string]json.RawMessage
	if err := strictDecode(body, &got); err != nil {
		return err
	}
	var errs []error
	for _, k := range keys {
		if _, ok := got[k]; !ok {
			errs = append(errs, fmt.Errorf("missing %q", k))
		}
	}
	if err := strictDecode(body, v); err != nil {
		errs = append(errs, err)
	}
	for k, raw := range got {
		if string(raw) == "null" {
			errs = append(errs, fmt.Errorf("%q is null", k))
		}
	}
	return errors.Join(errs...)
}

func strictDecode(s string, v any) error {
	dec := json.NewDecoder(strings.NewReader(s))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("not the expected JSON object: %w", err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return errors.New("text after the JSON object")
	}
	return nil
}

func (r Result) validate() error {
	var errs []error
	switch r.Decision {
	case Ready:
		if len(r.AcceptanceCriteria) == 0 {
			errs = append(errs, errors.New("ready without acceptance criteria"))
		}
	case NeedsInfo:
		if len(r.Questions) == 0 || len(r.Questions) > 3 {
			errs = append(errs, fmt.Errorf("needs_info with %d questions, want 1 to 3", len(r.Questions)))
		}
	case OutOfScope:
		if strings.TrimSpace(r.OutOfScopeReason) == "" {
			errs = append(errs, errors.New("out_of_scope without a reason"))
		}
	default:
		errs = append(errs, fmt.Errorf("decision %q", r.Decision))
	}
	if strings.TrimSpace(r.Summary) == "" {
		errs = append(errs, errors.New("empty summary"))
	}
	if r.Size != "S" && r.Size != "M" && r.Size != "L" {
		errs = append(errs, fmt.Errorf("size %q", r.Size))
	}
	if r.Suspicious && len(r.SuspiciousReasons) == 0 {
		errs = append(errs, errors.New("suspicious without reasons"))
	}
	for name, list := range map[string][]string{
		"acceptance_criteria": r.AcceptanceCriteria, "questions": r.Questions,
		"touches": r.Touches, "risks": r.Risks, "suspicious_reasons": r.SuspiciousReasons,
	} {
		for _, s := range list {
			if strings.TrimSpace(s) == "" {
				errs = append(errs, fmt.Errorf("empty entry in %s", name))
			}
		}
	}
	return errors.Join(errs...)
}
