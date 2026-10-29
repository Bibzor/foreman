package config_test

import (
	"errors"
	"io/fs"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Bibzor/foreman/internal/config"
)

const valid = `
app_id: 42
private_key_path: ~/.config/foreman/app.pem
repos: ["Bibzor/demo-api", "Bibzor/demo-web"]
authorized_users: ["Bibzor"]
`

func TestParseDefaults(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	c, err := config.Parse([]byte(valid))
	if err != nil {
		t.Fatal(err)
	}
	if c.AppID != 42 {
		t.Errorf("AppID = %d", c.AppID)
	}
	if want := filepath.Join(home, ".config/foreman/app.pem"); c.PrivateKeyPath != want {
		t.Errorf("PrivateKeyPath = %q, want %q", c.PrivateKeyPath, want)
	}
	if want := filepath.Join(home, ".local/share/foreman"); c.DataDir != want {
		t.Errorf("DataDir = %q, want %q", c.DataDir, want)
	}
	if c.PollInterval != 30*time.Second {
		t.Errorf("PollInterval = %s", c.PollInterval)
	}
	if c.WIPLimit != 2 {
		t.Errorf("WIPLimit = %d", c.WIPLimit)
	}
	if len(c.Repos) != 2 || c.Repos[1].String() != "Bibzor/demo-web" {
		t.Errorf("Repos = %v", c.Repos)
	}
	if !c.IsAuthorized("bibzor") || c.IsAuthorized("mallory") {
		t.Error("IsAuthorized is wrong")
	}
}

func TestParseOverrides(t *testing.T) {
	c, err := config.Parse([]byte(valid + "poll_interval: 5m\ndata_dir: /var/lib/foreman\nwip_limit: 5\n"))
	if err != nil {
		t.Fatal(err)
	}
	if c.PollInterval != 5*time.Minute || c.DataDir != "/var/lib/foreman" || c.WIPLimit != 5 {
		t.Errorf("got %s %q %d", c.PollInterval, c.DataDir, c.WIPLimit)
	}
}

func TestParseInvalid(t *testing.T) {
	tests := []struct {
		name string
		yaml string
		want string
	}{
		{"empty", "{}", "app_id is required"},
		{"unknown key", valid + "token: x\n", "field token not found"},
		{"bad repo", strings.Replace(valid, "Bibzor/demo-web", "../etc", 1), `"../etc" is not owner/name`},
		{"dot repo", strings.Replace(valid, "Bibzor/demo-web", "Bibzor/..", 1), `is not owner/name`},
		{"nested repo", strings.Replace(valid, "Bibzor/demo-web", "a/b/c", 1), `is not owner/name`},
		{"duplicate repo", strings.Replace(valid, "Bibzor/demo-web", "bibzor/demo-api", 1), "listed twice"},
		{"bad login", strings.Replace(valid, `["Bibzor"]`, `["a b"]`, 1), "is not a GitHub login"},
		{"bad interval", valid + "poll_interval: soon\n", "poll_interval"},
		{"tiny interval", valid + "poll_interval: 1ms\n", "below 1s"},
		{"zero wip limit", valid + "wip_limit: 0\n", "wip_limit: 0 is below 1"},
		{"relative data dir", valid + "data_dir: data\n", "must be absolute"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := config.Parse([]byte(tt.yaml))
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("err = %v, want %q", err, tt.want)
			}
		})
	}
}

func TestLoadMissing(t *testing.T) {
	_, err := config.Load(filepath.Join(t.TempDir(), "nope.yml"))
	if !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("err = %v", err)
	}
}
