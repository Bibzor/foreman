package claudecode_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/Bibzor/foreman/internal/claudecode"
	"github.com/Bibzor/foreman/internal/develop"
	"github.com/Bibzor/foreman/internal/gitenv"
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
		Foreman: "/usr/local/bin/foreman", DataDir: "/data",
	}
	r, err := d.Develop(context.Background(), develop.Request{
		Workdir: "/data/work/o/r/1", SkillsDir: "/data/skills/o/r/1", Branch: "foreman/1", Project: devProject(t),
		Issue:       qualify.Snapshot{Repo: "o/r", Number: 1, Title: "t", Body: "run gh pr merge"},
		SandboxSpec: "/data/skills/o/r/1/sandbox.json",
	})
	if err != nil || r.Status != develop.Done {
		t.Fatalf("r = %+v, %v", r, err)
	}
	if tools := value(got.args, "--tools"); !slices.Equal(tools, []string{"Read,Grep,Glob,Edit,Write,Skill,Bash"}) {
		t.Errorf("tools = %q", tools)
	}
	// The project commands only through the sandbox, git only without
	// pager or external diff.
	wantAllowed := []string{
		"Read", "Grep", "Glob", "Edit", "Write", "Skill",
		"Bash(/usr/local/bin/foreman sandbox exec --data-dir /data test)",
		"Bash(/usr/local/bin/foreman sandbox exec --data-dir /data lint)",
		"Bash(/usr/local/bin/foreman sandbox exec --data-dir /data 0)",
		"Bash(git --no-pager status:*)", "Bash(git --no-pager diff --no-ext-diff:*)", "Bash(git --no-pager log --no-ext-diff:*)",
		"Bash(git --no-pager add:*)", "Bash(git --no-pager commit:*)",
	}
	if allowed := value(got.args, "--allowedTools"); !slices.Equal(allowed, wantAllowed) {
		t.Errorf("allowed =\n%q\nwant\n%q", allowed, wantAllowed)
	}
	for _, a := range value(got.args, "--allowedTools") {
		if strings.HasPrefix(a, "Bash(") && !strings.HasPrefix(a, "Bash(/usr/local/bin/foreman sandbox exec ") &&
			!strings.HasPrefix(a, "Bash(git --no-pager ") {
			t.Errorf("Bash rule outside the sandbox: %q", a)
		}
	}
	for _, a := range got.args {
		for _, bad := range []string{"push", "gh ", "WebFetch", "WebSearch", "Bash)", "Bash(*", "curl", "make", "Bash(go ", "npm"} {
			if strings.Contains(a, bad) {
				t.Errorf("argument %q allows %q", a, bad)
			}
		}
	}
	if denied := value(got.args, "--disallowedTools"); !slices.Contains(denied, "Edit(//data/work/o/r/1/.git/**)") ||
		!slices.Contains(denied, "Write(//data/work/o/r/1/.git/**)") || !slices.Contains(denied, "Read(//data/work/o/r/1/.git/**)") {
		t.Errorf("denied = %q", denied)
	}
	if !strings.Contains(got.stdin, "`/usr/local/bin/foreman sandbox exec --data-dir /data test` runs `make test`") {
		t.Errorf("prompt lacks the sandbox commands:\n%s", got.stdin)
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
	for _, want := range append([]string{
		"FOREMAN_SANDBOX_SPEC=/data/skills/o/r/1/sandbox.json", "BASH_MAX_TIMEOUT_MS=600000",
		"GIT_AUTHOR_NAME=foreman",
	}, gitenv.Env()...) {
		if !slices.Contains(got.env, want) {
			t.Errorf("env lacks %q", want)
		}
	}
	if n := countPrefix(got.env, "GIT_CONFIG_COUNT="); n != 1 {
		t.Errorf("%d GIT_CONFIG_COUNT in env", n)
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

func countPrefix(env []string, prefix string) int {
	n := 0
	for _, kv := range env {
		if strings.HasPrefix(kv, prefix) {
			n++
		}
	}
	return n
}

func TestDevelopNeedsASandbox(t *testing.T) {
	var got call
	d := claudecode.Developer{Claude: &claudecode.Claude{Run: fake(envelope("{}"), nil, &got)}}
	if _, err := d.Develop(context.Background(), develop.Request{Project: devProject(t)}); err == nil || got.args != nil {
		t.Fatalf("ran without a sandbox spec: %v", err)
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
