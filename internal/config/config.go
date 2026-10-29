// Package config loads the foreman configuration file.
//
// It holds the path of the GitHub App private key, never the key itself:
// only internal/github reads it.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"
)

// Defaults.
const (
	DefaultPollInterval = 30 * time.Second
	DefaultWIPLimit     = 2
	defaultPath         = "~/.config/foreman/config.yml"
	defaultDataDir      = "~/.local/share/foreman"
)

// Config is the validated configuration.
type Config struct {
	AppID           int64
	PrivateKeyPath  string
	Repos           []Repo
	AuthorizedUsers []string
	PollInterval    time.Duration
	DataDir         string
	// WIPLimit is how many issues may be in development or in review at
	// once, across every repository.
	WIPLimit int
}

// Repo is a GitHub repository.
type Repo struct {
	Owner string
	Name  string
}

func (r Repo) String() string { return r.Owner + "/" + r.Name }

// file is the YAML shape of the configuration file.
type file struct {
	AppID           int64    `yaml:"app_id"`
	PrivateKeyPath  string   `yaml:"private_key_path"`
	Repos           []string `yaml:"repos"`
	AuthorizedUsers []string `yaml:"authorized_users"`
	PollInterval    string   `yaml:"poll_interval"`
	DataDir         string   `yaml:"data_dir"`
	WIPLimit        *int     `yaml:"wip_limit"`
}

// GitHub owner and repository names, restricted to characters that are
// safe in a filesystem path: repos become directories under data_dir.
var (
	ownerRe = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9-]{0,38})$`)
	nameRe  = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,100}$`)
	loginRe = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9-]{0,38})$`)
)

// DefaultPath returns the default configuration path, with ~ expanded.
func DefaultPath() (string, error) { return expandHome(defaultPath) }

// Load reads and validates the configuration file at path.
func Load(path string) (Config, error) {
	b, err := os.ReadFile(path) //nolint:gosec // the path is chosen by the operator
	if err != nil {
		return Config{}, fmt.Errorf("read config: %w", err)
	}
	return Parse(b)
}

// Parse validates a configuration file's content and applies defaults.
func Parse(b []byte) (Config, error) {
	var f file
	dec := yaml.NewDecoder(bytes.NewReader(b))
	dec.KnownFields(true)
	if err := dec.Decode(&f); err != nil {
		return Config{}, fmt.Errorf("parse config: %w", err)
	}

	c := Config{AppID: f.AppID, PollInterval: DefaultPollInterval, WIPLimit: DefaultWIPLimit}
	var errs []error
	if f.AppID <= 0 {
		errs = append(errs, errors.New("app_id is required"))
	}
	if f.PrivateKeyPath == "" {
		errs = append(errs, errors.New("private_key_path is required"))
	} else if p, err := expandHome(f.PrivateKeyPath); err != nil {
		errs = append(errs, err)
	} else {
		c.PrivateKeyPath = p
	}
	if len(f.Repos) == 0 {
		errs = append(errs, errors.New("repos is required"))
	}
	seen := make(map[string]bool)
	for _, r := range f.Repos {
		owner, name, ok := strings.Cut(r, "/")
		if !ok || !ownerRe.MatchString(owner) || !nameRe.MatchString(name) || name == "." || name == ".." {
			errs = append(errs, fmt.Errorf("repos: %q is not owner/name", r))
			continue
		}
		key := strings.ToLower(r)
		if seen[key] {
			errs = append(errs, fmt.Errorf("repos: %q is listed twice", r))
			continue
		}
		seen[key] = true
		c.Repos = append(c.Repos, Repo{Owner: owner, Name: name})
	}
	if len(f.AuthorizedUsers) == 0 {
		errs = append(errs, errors.New("authorized_users is required"))
	}
	for _, u := range f.AuthorizedUsers {
		if !loginRe.MatchString(u) {
			errs = append(errs, fmt.Errorf("authorized_users: %q is not a GitHub login", u))
			continue
		}
		c.AuthorizedUsers = append(c.AuthorizedUsers, u)
	}
	if f.PollInterval != "" {
		d, err := time.ParseDuration(f.PollInterval)
		switch {
		case err != nil:
			errs = append(errs, fmt.Errorf("poll_interval: %w", err))
		case d < time.Second:
			errs = append(errs, fmt.Errorf("poll_interval: %s is below 1s", d))
		default:
			c.PollInterval = d
		}
	}
	if f.WIPLimit != nil {
		if *f.WIPLimit < 1 {
			errs = append(errs, fmt.Errorf("wip_limit: %d is below 1", *f.WIPLimit))
		} else {
			c.WIPLimit = *f.WIPLimit
		}
	}
	dataDir := f.DataDir
	if dataDir == "" {
		dataDir = defaultDataDir
	}
	if p, err := expandHome(dataDir); err != nil {
		errs = append(errs, err)
	} else if !filepath.IsAbs(p) {
		errs = append(errs, fmt.Errorf("data_dir: %q must be absolute", dataDir))
	} else {
		c.DataDir = p
	}
	if err := errors.Join(errs...); err != nil {
		return Config{}, fmt.Errorf("invalid config: %w", err)
	}
	return c, nil
}

// IsAuthorized reports whether login is an authorized user. GitHub logins
// are case-insensitive.
func (c Config) IsAuthorized(login string) bool {
	for _, u := range c.AuthorizedUsers {
		if strings.EqualFold(u, login) {
			return true
		}
	}
	return false
}

func expandHome(p string) (string, error) {
	if p != "~" && !strings.HasPrefix(p, "~/") {
		return p, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("expand %q: %w", p, err)
	}
	return filepath.Join(home, strings.TrimPrefix(p, "~")), nil
}
