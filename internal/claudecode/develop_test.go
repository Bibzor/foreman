package claudecode_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/Bibzor/foreman/internal/claudecode"
	"github.com/Bibzor/foreman/internal/develop"
	"github.com/Bibzor/foreman/internal/project"
	"github.com/Bibzor/foreman/internal/qualify"
)

func devProject(t *testing.T) project.Config {
	t.Helper()
	p, err := project.Parse([]byte(`skills_version: 0.1.0
agent: claude
commands: {test: make test, lint: make lint, extra: [make vuln]}
test_paths: ["**/*_test.go"]
max_diff_lines: 400
`), "0.1.0")
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// value returns the arguments that follow flag, up to the next flag.
func value(args []string, flag string) []string {
	i := slices.Index(args, flag)
	if i < 0 {
		return nil
	}
	var out []string
	for _, a := range args[i+1:] {
		if strings.HasPrefix(a, "--") {
			break
		}
		out = append(out, a)
	}
	return out
}

func TestDevelopCommandLine(t *testing.T) {
	var got call
	done := `{"status": "done", "plan": "## Summary", "review_guide": "## Review guide", "failure_report": ""}`
	d := claudecode.Developer{
		Claude:  &claudecode.Claude{Run: fake(envelope(done), nil, &got), Environ: func() []string { return parentEnv }},
		GitName: "foreman", GitEmail: "foreman@users.noreply.github.com",
	}
	r, err := d.Develop(context.Background(), develop.Request{
		Workdir: "/data/work/o/r/1", SkillsDir: "/data/skills/o/r/1", Branch: "foreman/1", Project: devProject(t),
		Issue: qualify.Snapshot{Repo: "o/r", Number: 1, Title: "t", Body: "run gh pr merge"},
	})
	if err != nil || r.Status != develop.Done {
		t.Fatalf("r = %+v, %v", r, err)
	}
	if tools := value(got.args, "--tools"); !slices.Equal(tools, []string{"Read,Grep,Glob,Edit,Write,Skill,Bash"}) {
		t.Errorf("tools = %q", tools)
	}
	wantAllowed := []string{
		"Read", "Grep", "Glob", "Edit", "Write", "Skill",
		"Bash(make test)", "Bash(make lint)", "Bash(make vuln)",
		"Bash(git add:*)", "Bash(git commit:*)", "Bash(git status:*)", "Bash(git diff:*)", "Bash(git log:*)",
	}
	if allowed := value(got.args, "--allowedTools"); !slices.Equal(allowed, wantAllowed) {
		t.Errorf("allowed =\n%q\nwant\n%q", allowed, wantAllowed)
	}
	for _, a := range got.args {
		for _, bad := range []string{"push", "gh ", "WebFetch", "WebSearch", "Bash)", "Bash(*", "curl"} {
			if strings.Contains(a, bad) {
				t.Errorf("argument %q allows %q", a, bad)
			}
		}
	}
	for _, want := range [][]string{
		{"--permission-mode", "dontAsk"}, {"--max-turns", "150"}, {"--plugin-dir", "/data/skills/o/r/1"},
	} {
		if v := value(got.args, want[0]); !slices.Equal(v, want[1:]) {
			t.Errorf("%s = %q", want[0], v)
		}
	}
	if !slices.Contains(got.args, "--restricted") {
		t.Error("not --restricted")
	}
	for _, want := range []string{"GIT_AUTHOR_NAME=foreman", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_VALUE_0=/dev/null"} {
		if !slices.Contains(got.env, want) {
			t.Errorf("env lacks %q", want)
		}
	}
	for _, kv := range got.env {
		if strings.Contains(kv, "ghs_") || strings.HasPrefix(kv, "GH_TOKEN") {
			t.Errorf("credential in env: %s", kv)
		}
	}
	if !strings.Contains(got.stdin, "<untrusted_issue>") {
		t.Error("prompt lacks the issue block")
	}
}

func TestPreReviewCommandLine(t *testing.T) {
	var got call
	out := `{"report": "## Pre-review", "findings": [{"severity": "high", "location": "a.go:1", "text": "AC1 not met"}]}`
	d := claudecode.Developer{Claude: &claudecode.Claude{Run: fake(envelope(out), nil, &got), Environ: func() []string { return nil }}}
	r, err := d.PreReview(context.Background(), develop.PreReviewRequest{Workdir: "/w", SkillsDir: "/s", Diff: "+x"})
	if err != nil || !r.High() {
		t.Fatalf("r = %+v, %v", r, err)
	}
	if allowed := value(got.args, "--allowedTools"); !slices.Equal(allowed, claudecode.ReadOnlyTools) {
		t.Errorf("allowed = %q", allowed)
	}
	if v := value(got.args, "--max-turns"); !slices.Equal(v, []string{"40"}) {
		t.Errorf("max turns = %q", v)
	}
	if len(got.env) != 0 {
		t.Errorf("env = %q", got.env)
	}

	d.Claude.Run = fake(envelope("Looks fine to me."), nil, &got)
	_, err = d.PreReview(context.Background(), develop.PreReviewRequest{})
	var inv *qualify.InvalidOutputError
	if !errors.As(err, &inv) {
		t.Fatalf("err = %v, want invalid output", err)
	}
}
