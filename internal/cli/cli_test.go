package cli_test

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Bibzor/foreman/internal/cli"
)

func run(args ...string) (code int, stdout, stderr string) {
	var out, errOut bytes.Buffer
	code = cli.Run(args, &out, &errOut)
	return code, out.String(), errOut.String()
}

func TestRun(t *testing.T) {
	target := t.TempDir()
	tests := []struct {
		name       string
		args       []string
		wantCode   int
		wantStdout string // substring
		wantStderr string // substring
	}{
		{name: "version", args: []string{"version"}, wantCode: 0, wantStdout: "foreman dev\n"},
		{name: "help", args: []string{"help"}, wantCode: 0, wantStdout: "Usage:"},
		{name: "-h", args: []string{"-h"}, wantCode: 0, wantStdout: "Usage:"},
		{name: "no args", args: nil, wantCode: 2, wantStderr: "Usage:"},
		{name: "unknown command", args: []string{"deploy"}, wantCode: 2, wantStderr: `unknown command "deploy"`},
		{name: "version extra arg", args: []string{"version", "x"}, wantCode: 2, wantStderr: "unexpected argument"},
		{name: "skills no sub", args: []string{"skills"}, wantCode: 2, wantStderr: "Usage:"},
		{name: "skills unknown sub", args: []string{"skills", "remove"}, wantCode: 2, wantStderr: `unknown command "skills remove"`},
		{name: "list", args: []string{"skills", "list"}, wantCode: 0, wantStdout: "foreman-qualify"},
		{name: "install without target", args: []string{"skills", "install", "--agent", "claude"}, wantCode: 2, wantStderr: "--target is required"},
		{name: "install bad flag", args: []string{"skills", "install", "--nope"}, wantCode: 2, wantStderr: "flag provided but not defined"},
		{name: "install codex", args: []string{"skills", "install", "--agent", "codex", "--target", target}, wantCode: 1, wantStderr: `agent "codex" is not supported yet`},
		{name: "install opencode", args: []string{"skills", "install", "--agent", "opencode", "--target", target}, wantCode: 1, wantStderr: `agent "opencode" is not supported yet`},
		{name: "verify without target", args: []string{"skills", "verify"}, wantCode: 2, wantStderr: "--target is required"},
		{name: "verify before install", args: []string{"skills", "verify", "--target", target}, wantCode: 1, wantStderr: "foreman skills install"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			code, stdout, stderr := run(tt.args...)
			if code != tt.wantCode {
				t.Errorf("exit code = %d, want %d\nstdout: %s\nstderr: %s", code, tt.wantCode, stdout, stderr)
			}
			if !strings.Contains(stdout, tt.wantStdout) {
				t.Errorf("stdout = %q, want it to contain %q", stdout, tt.wantStdout)
			}
			if !strings.Contains(stderr, tt.wantStderr) {
				t.Errorf("stderr = %q, want it to contain %q", stderr, tt.wantStderr)
			}
		})
	}
}

func TestInstallThenVerify(t *testing.T) {
	target := t.TempDir()

	if code, _, stderr := run("skills", "install", "--agent", "claude", "--target", target); code != 0 {
		t.Fatalf("install exit code = %d, stderr: %s", code, stderr)
	}
	if code, stdout, stderr := run("skills", "verify", "--target", target); code != 0 {
		t.Fatalf("verify exit code = %d\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}

	const skill = ".claude/skills/foreman-plan/SKILL.md"
	p := filepath.Join(target, skill)
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	b[0] ^= 1
	if err := os.WriteFile(p, b, 0o600); err != nil {
		t.Fatal(err)
	}

	code, stdout, _ := run("skills", "verify", "--target", target)
	if code != 1 {
		t.Errorf("verify after modification exit code = %d, want 1", code)
	}
	if want := "modified: " + skill; !strings.Contains(stdout, want) {
		t.Errorf("stdout = %q, want it to contain %q", stdout, want)
	}
}

func TestInstallDefaultsToClaude(t *testing.T) {
	target := t.TempDir()
	if code, _, stderr := run("skills", "install", "--target", target); code != 0 {
		t.Fatalf("install exit code = %d, stderr: %s", code, stderr)
	}
	if _, err := os.Stat(filepath.Join(target, ".claude/skills/foreman-qualify/SKILL.md")); err != nil {
		t.Error(err)
	}
}
