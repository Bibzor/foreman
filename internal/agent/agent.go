// Package agent maps coding agents to the directory where they read skills.
package agent

import (
	"errors"
	"fmt"
	"sort"
	"strings"
)

// Errors returned by SkillsDir.
var (
	ErrUnknown      = errors.New("unknown agent")
	ErrNotSupported = errors.New("not supported yet")
)

// dirs is the only place that knows where each agent reads skills, as a
// slash-separated path relative to the repository root. An empty value
// means the agent is known but not supported yet: adding one is filling
// in its directory here.
var dirs = map[string]string{
	"claude":   ".claude/skills",
	"codex":    "",
	"opencode": "",
}

// SkillsDir returns the skills directory of the named agent.
func SkillsDir(name string) (string, error) {
	dir, ok := dirs[name]
	switch {
	case !ok:
		return "", fmt.Errorf("%w %q (known: %s)", ErrUnknown, name, strings.Join(names(false), ", "))
	case dir == "":
		return "", fmt.Errorf("agent %q is %w (supported: %s)", name, ErrNotSupported, strings.Join(Supported(), ", "))
	}
	return dir, nil
}

// Supported returns the names of the supported agents, sorted.
func Supported() []string { return names(true) }

func names(supportedOnly bool) []string {
	var out []string
	for name, dir := range dirs {
		if !supportedOnly || dir != "" {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}
