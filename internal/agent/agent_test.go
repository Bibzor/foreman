package agent

import (
	"errors"
	"testing"
)

func TestSkillsDir(t *testing.T) {
	tests := []struct {
		agent   string
		want    string
		wantErr error
	}{
		{agent: "claude", want: ".claude/skills"},
		{agent: "codex", wantErr: ErrNotSupported},
		{agent: "opencode", wantErr: ErrNotSupported},
		{agent: "vim", wantErr: ErrUnknown},
		{agent: "", wantErr: ErrUnknown},
	}
	for _, tt := range tests {
		t.Run(tt.agent, func(t *testing.T) {
			got, err := SkillsDir(tt.agent)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("SkillsDir(%q) error = %v, want %v", tt.agent, err, tt.wantErr)
			}
			if got != tt.want {
				t.Errorf("SkillsDir(%q) = %q, want %q", tt.agent, got, tt.want)
			}
		})
	}
}

func TestSkillsDirErrorMessage(t *testing.T) {
	_, err := SkillsDir("codex")
	want := `agent "codex" is not supported yet (supported: claude)`
	if err == nil || err.Error() != want {
		t.Errorf("error = %v, want %q", err, want)
	}
}
