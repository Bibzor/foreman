// Package gitenv is the environment of every git process foreman starts,
// for itself or for the agent.
//
// A clone's .git/config is not trusted: an agent, or code it ran, may
// have written it. Options such as core.fsmonitor, core.pager,
// diff.external or core.hooksPath make git execute a command, so they are
// forced through GIT_CONFIG_COUNT, which takes precedence over every
// configuration file. The system and global files are not read at all.
//
// Options git cannot be forced on in advance, such as filter.<name>.clean,
// are kept out by never letting anything write .git/config: the agent's
// file tools are denied .git, and the sandbox mounts it read-only.
package gitenv

import (
	"os"
	"strconv"
)

// Pair is one configuration option.
type Pair struct{ Key, Value string }

// Forced are the options set on every git process, over any
// configuration file.
var Forced = []Pair{
	{"core.fsmonitor", "false"},
	{"core.hooksPath", os.DevNull},
	{"core.pager", "cat"},
	// git cannot unset an option, and an empty diff.external fails: true
	// makes an external diff print nothing. Patches are asked for with
	// --no-ext-diff, which ignores it.
	{"diff.external", "true"},
	{"core.editor", "true"},
	{"sequence.editor", "true"},
	{"commit.gpgSign", "false"},
	{"tag.gpgSign", "false"},
}

// Env returns the variables that give git no system or global
// configuration, the Forced options, then extra (such as a credential
// header). It replaces any GIT_CONFIG_COUNT the caller would set: there
// can be only one.
func Env(extra ...Pair) []string {
	pairs := append(append([]Pair{}, Forced...), extra...)
	env := []string{
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL=" + os.DevNull,
		"GIT_PAGER=cat",
		"GIT_CONFIG_COUNT=" + strconv.Itoa(len(pairs)),
	}
	for i, p := range pairs {
		n := strconv.Itoa(i)
		env = append(env, "GIT_CONFIG_KEY_"+n+"="+p.Key, "GIT_CONFIG_VALUE_"+n+"="+p.Value)
	}
	return env
}
