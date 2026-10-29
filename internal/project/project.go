// Package project reads .foreman.yml, the configuration a project commits
// at the root of its default branch.
//
// The commands it lists are run by foreman and allowed to the agent. They
// are run as argv, never through a shell, so they are restricted to plain
// words: no quoting, no variables, no redirection, no globbing.
package project

import (
	"bytes"
	"errors"
	"fmt"
	"path"
	"regexp"
	"strings"

	"go.yaml.in/yaml/v3"

	"github.com/Bibzor/foreman/internal/agent"
)

// File is the name of the project configuration, at the repository root.
const File = ".foreman.yml"

// Config is a validated .foreman.yml.
type Config struct {
	SkillsVersion  string
	Agent          string
	Test           Command
	Lint           Command
	Extra          []Command
	ProtectedPaths []string
	TestPaths      []string
	MaxDiffLines   int
}

// Command is a command run as argv.
type Command struct {
	Raw  string // as written, with single spaces
	Argv []string
}

func (c Command) String() string { return c.Raw }

// Commands returns every command the agent may run: test, lint, extra.
func (c Config) Commands() []Command {
	return append([]Command{c.Test, c.Lint}, c.Extra...)
}

type file struct {
	SkillsVersion string `yaml:"skills_version"`
	Agent         string `yaml:"agent"`
	Commands      struct {
		Test  string   `yaml:"test"`
		Lint  string   `yaml:"lint"`
		Extra []string `yaml:"extra"`
	} `yaml:"commands"`
	ProtectedPaths []string `yaml:"protected_paths"`
	TestPaths      []string `yaml:"test_paths"`
	MaxDiffLines   int      `yaml:"max_diff_lines"`
}

// ErrMissing means the repository has no .foreman.yml.
var ErrMissing = errors.New(File + " is missing from the default branch")

// word is one argv element: letters, digits and a few punctuation marks
// that no shell or Claude Code permission rule gives a meaning to.
var word = regexp.MustCompile(`^[A-Za-z0-9_./:=@%+,-]+$`)

// Parse validates b. packVersion is the skills pack foreman ships: the
// project must pin exactly that version.
func Parse(b []byte, packVersion string) (Config, error) {
	if b == nil {
		return Config{}, ErrMissing
	}
	var f file
	dec := yaml.NewDecoder(bytes.NewReader(b))
	dec.KnownFields(true)
	if err := dec.Decode(&f); err != nil {
		return Config{}, fmt.Errorf("parse %s: %w", File, err)
	}

	c := Config{SkillsVersion: f.SkillsVersion, Agent: f.Agent, MaxDiffLines: f.MaxDiffLines}
	var errs []error
	switch {
	case f.SkillsVersion == "":
		errs = append(errs, errors.New("skills_version is required"))
	case f.SkillsVersion != packVersion:
		errs = append(errs, fmt.Errorf("project pins %s, foreman ships %s", f.SkillsVersion, packVersion))
	}
	if f.Agent == "" {
		errs = append(errs, errors.New("agent is required"))
	} else if _, err := agent.SkillsDir(f.Agent); err != nil {
		errs = append(errs, fmt.Errorf("agent: %w", err))
	} else if f.Agent != "claude" {
		errs = append(errs, fmt.Errorf("agent: foreman runs only claude for now, not %q", f.Agent))
	}
	var err error
	if c.Test, err = parseCommand("commands.test", f.Commands.Test); err != nil {
		errs = append(errs, err)
	}
	if c.Lint, err = parseCommand("commands.lint", f.Commands.Lint); err != nil {
		errs = append(errs, err)
	}
	for i, raw := range f.Commands.Extra {
		cmd, err := parseCommand(fmt.Sprintf("commands.extra[%d]", i), raw)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		c.Extra = append(c.Extra, cmd)
	}
	for _, p := range f.ProtectedPaths {
		clean, err := protectedPath(p)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		c.ProtectedPaths = append(c.ProtectedPaths, clean)
	}
	if len(f.TestPaths) == 0 {
		errs = append(errs, errors.New("test_paths is required"))
	}
	for _, g := range f.TestPaths {
		if err := validGlob(g); err != nil {
			errs = append(errs, fmt.Errorf("test_paths: %q: %w", g, err))
			continue
		}
		c.TestPaths = append(c.TestPaths, g)
	}
	if f.MaxDiffLines <= 0 {
		errs = append(errs, errors.New("max_diff_lines must be greater than 0"))
	}
	if err := errors.Join(errs...); err != nil {
		return Config{}, fmt.Errorf("invalid %s: %w", File, err)
	}
	return c, nil
}

func parseCommand(field, raw string) (Command, error) {
	if strings.TrimSpace(raw) == "" {
		return Command{}, fmt.Errorf("%s is required", field)
	}
	var clean []string
	for _, w := range strings.Split(strings.TrimSpace(raw), " ") {
		if w == "" {
			continue // repeated spaces
		}
		if !word.MatchString(w) {
			return Command{}, fmt.Errorf("%s: %q: only letters, digits and _./:=@%%+,- are allowed in a command, and no shell syntax", field, raw)
		}
		clean = append(clean, w)
	}
	if strings.HasPrefix(clean[0], "-") {
		return Command{}, fmt.Errorf("%s: %q must start with a program name", field, raw)
	}
	return Command{Raw: strings.Join(clean, " "), Argv: clean}, nil
}

// protectedPath accepts "dir/" (a prefix) or an exact relative path.
func protectedPath(p string) (string, error) {
	dir := strings.HasSuffix(p, "/")
	trimmed := strings.TrimSuffix(p, "/")
	if trimmed == "" || strings.HasPrefix(p, "/") || strings.ContainsAny(p, "\\*?[\n") {
		return "", fmt.Errorf("protected_paths: %q must be a relative path, with no glob", p)
	}
	clean := path.Clean(trimmed)
	if clean != trimmed || clean == "." || clean == ".." || strings.HasPrefix(clean, "../") {
		return "", fmt.Errorf("protected_paths: %q must be a clean relative path inside the repository", p)
	}
	if dir {
		return clean + "/", nil
	}
	return clean, nil
}

// IsProtected reports whether the repository path p is under a protected
// path.
func (c Config) IsProtected(p string) bool {
	for _, pp := range c.ProtectedPaths {
		if p == pp || (strings.HasSuffix(pp, "/") && strings.HasPrefix(p, pp)) {
			return true
		}
	}
	return false
}

// IsTestPath reports whether the repository path p matches test_paths.
func (c Config) IsTestPath(p string) bool {
	for _, g := range c.TestPaths {
		if Match(g, p) {
			return true
		}
	}
	return false
}
