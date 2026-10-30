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
	// Acceptance is the acceptance environment, nil when the project has
	// none.
	Acceptance *Acceptance
	// Sandbox is the container the project commands run in. It is
	// required to develop, nil when the section is missing.
	Sandbox *Sandbox
}

// Sandbox is the container image and setup the project commands run in.
type Sandbox struct {
	Image string // name:tag or name@sha256:digest, never latest
	// Setup runs with network access, before the agent, on the default
	// branch: it fills the caches.
	Setup []Command
	// Cache are the container paths that persist from setup to the
	// commands, read-only for the commands.
	Cache []string
	Env   map[string]string
}

// Acceptance is the acceptance environment foreman deploys a merged
// change to. foreman builds the deployment commands itself from these
// values: none of them is a command.
type Acceptance struct {
	Dockerfile    string // relative to the repository root
	BuildArgs     map[string]string
	ContainerPort int
	HostPort      int // bound to 127.0.0.1 only
	Env           map[string]string
	Smoke         []Smoke
}

// Smoke is a smoke check: GET Path must answer Status.
type Smoke struct {
	Path   string
	Status int
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
	Acceptance     *struct {
		Dockerfile    string            `yaml:"dockerfile"`
		BuildArgs     map[string]string `yaml:"build_args"`
		ContainerPort int               `yaml:"container_port"`
		HostPort      int               `yaml:"host_port"`
		Env           map[string]string `yaml:"env"`
		Smoke         []struct {
			Path   string `yaml:"path"`
			Status int    `yaml:"status"`
		} `yaml:"smoke"`
	} `yaml:"acceptance"`
	Sandbox *struct {
		Image string            `yaml:"image"`
		Setup []string          `yaml:"setup"`
		Cache []string          `yaml:"cache"`
		Env   map[string]string `yaml:"env"`
	} `yaml:"sandbox"`
}

// ErrMissing means the repository has no .foreman.yml.
var ErrMissing = errors.New(File + " is missing from the default branch")

// word is one argv element: letters, digits and a few punctuation marks
// that no shell or Claude Code permission rule gives a meaning to.
var word = regexp.MustCompile(`^[A-Za-z0-9_./:=@%+,-]+$`)

// Parse validates b for a step that runs an agent. packVersion is the
// skills pack foreman ships: the project must pin exactly that version.
func Parse(b []byte, packVersion string) (Config, error) { return parse(b, packVersion, true) }

// ParseNoPack validates b like Parse, except that skills_version may
// pin any version: for the steps that run no agent, such as deploying,
// the skills do not matter.
func ParseNoPack(b []byte) (Config, error) { return parse(b, "", false) }

func parse(b []byte, packVersion string, checkPack bool) (Config, error) {
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
	case checkPack && f.SkillsVersion != packVersion:
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
	if a := f.Acceptance; a != nil {
		acc := &Acceptance{Dockerfile: a.Dockerfile, ContainerPort: a.ContainerPort, HostPort: a.HostPort}
		if acc.Dockerfile == "" {
			acc.Dockerfile = "Dockerfile"
		}
		if clean, err := relativeFile(acc.Dockerfile); err != nil {
			errs = append(errs, fmt.Errorf("acceptance.dockerfile: %w", err))
		} else {
			acc.Dockerfile = clean
		}
		var err error
		if acc.BuildArgs, err = variables("acceptance.build_args", a.BuildArgs); err != nil {
			errs = append(errs, err)
		}
		if acc.Env, err = variables("acceptance.env", a.Env); err != nil {
			errs = append(errs, err)
		}
		if a.ContainerPort < 1 || a.ContainerPort > 65535 {
			errs = append(errs, errors.New("acceptance.container_port must be between 1 and 65535"))
		}
		if a.HostPort < 1024 || a.HostPort > 65535 {
			errs = append(errs, errors.New("acceptance.host_port must be between 1024 and 65535"))
		}
		if len(a.Smoke) == 0 || len(a.Smoke) > maxSmoke {
			errs = append(errs, fmt.Errorf("acceptance.smoke needs 1 to %d checks", maxSmoke))
		}
		for i, s := range a.Smoke {
			if !smokePath.MatchString(s.Path) {
				errs = append(errs, fmt.Errorf("acceptance.smoke[%d].path: %q must start with / and have no space or control character", i, s.Path))
			}
			if s.Status < 100 || s.Status > 599 {
				errs = append(errs, fmt.Errorf("acceptance.smoke[%d].status: %d is not an HTTP status", i, s.Status))
			}
			acc.Smoke = append(acc.Smoke, Smoke{Path: s.Path, Status: s.Status})
		}
		c.Acceptance = acc
	}
	if f.Sandbox != nil {
		sb, err := parseSandbox(f.Sandbox.Image, f.Sandbox.Setup, f.Sandbox.Cache, f.Sandbox.Env)
		errs = append(errs, err)
		c.Sandbox = sb
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

// ValidArgv checks argv like a command of .foreman.yml: plain words, a
// program first.
func ValidArgv(argv []string) error {
	if len(argv) == 0 {
		return errors.New("empty command")
	}
	for _, w := range argv {
		if !word.MatchString(w) {
			return fmt.Errorf("%q: only letters, digits and _./:=@%%+,- are allowed in a command", w)
		}
	}
	if strings.HasPrefix(argv[0], "-") {
		return fmt.Errorf("%q must start with a program name", strings.Join(argv, " "))
	}
	return nil
}

// ValidEnv checks environment variables like those of .foreman.yml.
func ValidEnv(field string, m map[string]string) error {
	_, err := variables(field, m)
	return err
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

const (
	maxSetup = 10
	maxCache = 8
)

// image is a docker image reference with a tag or a digest:
// [registry[:port]/]name[/name...](:tag|@sha256:digest).
var image = regexp.MustCompile(`^([a-z0-9]([a-z0-9.-]*[a-z0-9])?(:[0-9]{1,5})?/)?[a-z0-9]+([._-][a-z0-9]+)*(/[a-z0-9]+([._-][a-z0-9]+)*)*` +
	`(:[A-Za-z0-9_][A-Za-z0-9_.-]{0,127}|@sha256:[0-9a-f]{64})$`)

// cachePath is an absolute container path made of plain words.
var cachePath = regexp.MustCompile(`^(/[A-Za-z0-9_.@+-]+)+$`)

// ImageRef validates a docker image reference pinned by a tag other than
// latest, or by a digest.
func ImageRef(ref string) error {
	if !image.MatchString(ref) {
		return fmt.Errorf("%q must be an image name with a tag or a sha256 digest", ref)
	}
	if strings.HasSuffix(ref, ":latest") {
		return fmt.Errorf("%q: pin a version, not latest", ref)
	}
	return nil
}

func parseSandbox(img string, setup, cache []string, env map[string]string) (*Sandbox, error) {
	sb := &Sandbox{Image: img}
	var errs []error
	if err := ImageRef(img); err != nil {
		errs = append(errs, fmt.Errorf("sandbox.image: %w", err))
	}
	if len(setup) > maxSetup {
		errs = append(errs, fmt.Errorf("sandbox.setup: at most %d commands", maxSetup))
	}
	for i, raw := range setup {
		cmd, err := parseCommand(fmt.Sprintf("sandbox.setup[%d]", i), raw)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		sb.Setup = append(sb.Setup, cmd)
	}
	if len(cache) > maxCache {
		errs = append(errs, fmt.Errorf("sandbox.cache: at most %d paths", maxCache))
	}
	seen := map[string]bool{}
	for _, p := range cache {
		if err := CachePath(p); err != nil {
			errs = append(errs, fmt.Errorf("sandbox.cache: %w", err))
			continue
		}
		if seen[p] {
			errs = append(errs, fmt.Errorf("sandbox.cache: %q is listed twice", p))
			continue
		}
		seen[p] = true
		sb.Cache = append(sb.Cache, p)
	}
	var err error
	if sb.Env, err = variables("sandbox.env", env); err != nil {
		errs = append(errs, err)
	}
	return sb, errors.Join(errs...)
}

// CachePath validates a cache path: absolute and clean, inside neither
// /tmp nor .git, and none of /, /work and /tmp themselves. A path under
// /work, such as /work/node_modules, is mounted over the workspace.
func CachePath(p string) error {
	if !cachePath.MatchString(p) || path.Clean(p) != p {
		return fmt.Errorf("%q must be a clean absolute path of letters, digits and _.@+-", p)
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == "." || seg == ".." || seg == ".git" {
			return fmt.Errorf("%q: no ., .. or .git", p)
		}
	}
	if p == "/work" || p == "/tmp" || strings.HasPrefix(p, "/tmp/") {
		return fmt.Errorf("%q is the workspace or /tmp, not a cache", p)
	}
	return nil
}

const maxSmoke = 10

var (
	varName   = regexp.MustCompile(`^[A-Z_][A-Z0-9_]*$`)
	smokePath = regexp.MustCompile(`^/[^\s\x00-\x1f\x7f]*$`)
)

// variables validates environment variables or build arguments: names in
// upper case, values on one line.
func variables(field string, m map[string]string) (map[string]string, error) {
	var errs []error
	for k, v := range m {
		if !varName.MatchString(k) {
			errs = append(errs, fmt.Errorf("%s: name %q must match %s", field, k, varName))
		}
		if strings.ContainsAny(v, "\n\r\x00") {
			errs = append(errs, fmt.Errorf("%s: the value of %s must be on one line", field, k))
		}
	}
	if err := errors.Join(errs...); err != nil {
		return nil, err
	}
	return m, nil
}

// relativeFile accepts a clean relative path to a file inside the
// repository.
func relativeFile(p string) (string, error) {
	if strings.HasPrefix(p, "/") || strings.HasSuffix(p, "/") || strings.ContainsAny(p, "\\*?[\n\r\x00") {
		return "", fmt.Errorf("%q must be a relative path to a file, with no glob", p)
	}
	clean := path.Clean(p)
	if clean != p || clean == "." || clean == ".." || strings.HasPrefix(clean, "../") {
		return "", fmt.Errorf("%q must be a clean relative path inside the repository", p)
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
