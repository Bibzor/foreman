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
	"unicode"
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

// Parse extracts the final JSON object from the agent's output and
// validates it against the foreman-qualify output shape. Text before the
// object is ignored.
func Parse(text string) (Result, error) {
	var r Result
	if _, err := DecodeStrict(text, &r, resultKeys); err != nil {
		return Result{}, err
	}
	return r, r.validate()
}

// DecodeStrict takes the last top-level JSON object of an agent's output,
// bare or in a final ```json fence, and decodes it into v: every key of
// keys present and not null, no other key, right types. Nothing may follow
// the object. The text before it is returned as notes: it is untrusted.
func DecodeStrict(text string, v any, keys []string) (notes string, err error) {
	notes, body, err := splitJSON(text)
	if err != nil {
		return "", err
	}
	var got map[string]json.RawMessage
	if err := strictDecode(body, &got); err != nil {
		return "", err
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
	return strings.TrimSpace(notes), errors.Join(errs...)
}

// splitJSON separates the output into the text before the final JSON
// object and the object. When no final object is found, body is the whole
// text, for strictDecode to report what is wrong with it.
func splitJSON(text string) (notes, body string, err error) {
	t := strings.TrimRightFunc(text, unicode.IsSpace)
	if strings.HasSuffix(t, "```") {
		inner := t[:len(t)-3]
		// JSON strings cannot hold a raw newline, so a fence at the start
		// of a line is never inside the object.
		open := strings.LastIndex("\n"+inner, "\n```")
		if open < 0 {
			return "", "", errors.New("unterminated code fence")
		}
		nl := strings.IndexByte(inner[open:], '\n')
		if nl < 0 {
			return "", "", errors.New("unterminated code fence")
		}
		if lang := strings.TrimSpace(inner[open+3 : open+nl]); lang != "" && lang != "json" {
			return "", "", fmt.Errorf("code fence language %q", lang)
		}
		return inner[:open], strings.TrimSpace(inner[open+nl+1:]), nil
	}
	// The top-level object is the one starting at the first brace from
	// which the rest of the output is exactly one JSON value.
	for i := 0; i < len(t); i++ {
		if t[i] == '{' && json.Valid([]byte(t[i:])) {
			notes = t[:i]
			if strings.Count(notes, "```")%2 == 1 {
				return "", "", errors.New("unterminated code fence")
			}
			return notes, t[i:], nil
		}
	}
	return "", strings.TrimSpace(text), nil
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
