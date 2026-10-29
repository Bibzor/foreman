// Package cli implements the foreman command line.
//
// It uses the standard library flag package: a handful of subcommands with
// one or two flags each do not justify a CLI framework dependency.
package cli

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"

	"github.com/Bibzor/foreman/internal/skillpack"
	"github.com/Bibzor/foreman/internal/version"
	"github.com/Bibzor/foreman/skills"
)

// Exit codes.
const (
	exitOK    = 0
	exitFail  = 1
	exitUsage = 2
)

const usage = `Usage:
  foreman run [--config <path>] [--once]
  foreman skills install [--agent claude] --target <repo>
  foreman skills verify --target <repo>
  foreman skills list
  foreman version
`

// errUsage marks errors caused by a bad command line.
var errUsage = errors.New("usage")

// Run runs foreman with args (without the program name) and returns the
// process exit code.
func Run(args []string, stdout, stderr io.Writer) int {
	err := dispatch(args, stdout, stderr)
	switch {
	case err == nil:
		return exitOK
	case errors.Is(err, flag.ErrHelp):
		return exitOK
	case errors.Is(err, errUsage):
		_, _ = fmt.Fprintf(stderr, "foreman: %v\n\n%s", err, usage) // nowhere left to report a failure
		return exitUsage
	default:
		_, _ = fmt.Fprintf(stderr, "foreman: %v\n", err) // nowhere left to report a failure
		return exitFail
	}
}

func dispatch(args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		return fmt.Errorf("%w: missing command", errUsage)
	}
	switch args[0] {
	case "help", "-h", "-help", "--help":
		_, err := fmt.Fprint(stdout, usage)
		return err
	case "version":
		if err := parse("version", args[1:], stderr); err != nil {
			return err
		}
		_, err := fmt.Fprintf(stdout, "foreman %s\n", version.Version)
		return err
	case "run":
		return run(args[1:], stderr)
	case "skills":
		if len(args) < 2 {
			return fmt.Errorf("%w: missing skills command", errUsage)
		}
		switch args[1] {
		case "install":
			return install(args[2:], stdout, stderr)
		case "verify":
			return verify(args[2:], stdout, stderr)
		case "list":
			return list(args[2:], stdout, stderr)
		}
		return fmt.Errorf("%w: unknown command \"skills %s\"", errUsage, args[1])
	}
	return fmt.Errorf("%w: unknown command %q", errUsage, args[0])
}

func install(args []string, stdout, stderr io.Writer) error {
	var agentName, target string
	err := parse("skills install", args, stderr, func(fs *flag.FlagSet) {
		fs.StringVar(&agentName, "agent", "claude", "agent to install the skills for")
		fs.StringVar(&target, "target", "", "path of the target repository (required)")
	})
	if err != nil {
		return err
	}
	if target == "" {
		return fmt.Errorf("%w: --target is required", errUsage)
	}
	lock, err := skillpack.Install(skills.FS, target, agentName, version.Version)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(stdout, "installed %d files (skills %s) for %s in %s\n", len(lock.Files), lock.Version, lock.Agent, target)
	return err
}

func verify(args []string, stdout, stderr io.Writer) error {
	var target string
	err := parse("skills verify", args, stderr, func(fs *flag.FlagSet) {
		fs.StringVar(&target, "target", "", "path of the target repository (required)")
	})
	if err != nil {
		return err
	}
	if target == "" {
		return fmt.Errorf("%w: --target is required", errUsage)
	}
	r, err := skillpack.Verify(skills.FS, target, version.Version)
	if err != nil {
		return err
	}
	if r.OK() {
		_, err := fmt.Fprintf(stdout, "ok: skills %s installed in %s match this binary\n", r.BinaryVersion, target)
		return err
	}
	var b strings.Builder
	if r.LockVersion != r.BinaryVersion {
		fmt.Fprintf(&b, "version: installed %s, binary %s\n", r.LockVersion, r.BinaryVersion)
	}
	for _, group := range []struct {
		label string
		paths []string
	}{{"missing", r.Missing}, {"modified", r.Modified}, {"extra", r.Extra}} {
		for _, p := range group.paths {
			fmt.Fprintf(&b, "%s: %s\n", group.label, p)
		}
	}
	if _, err := io.WriteString(stdout, b.String()); err != nil {
		return err
	}
	return errors.New("skills do not match this binary (run foreman skills install to restore them)")
}

func list(args []string, stdout, stderr io.Writer) error {
	if err := parse("skills list", args, stderr); err != nil {
		return err
	}
	all, err := skillpack.List(skills.FS)
	if err != nil {
		return err
	}
	w := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
	for _, s := range all {
		if _, err := fmt.Fprintf(w, "%s\t%s\n", s.Name, s.Description); err != nil {
			return err
		}
	}
	return w.Flush()
}

// parse parses args for the named command with the flags that define adds,
// and rejects positional arguments.
func parse(name string, args []string, stderr io.Writer, define ...func(*flag.FlagSet)) error {
	fs := flag.NewFlagSet("foreman "+name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	for _, d := range define {
		d(fs)
	}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return err
		}
		return fmt.Errorf("%w: %w", errUsage, err)
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("%w: unexpected argument %q", errUsage, strings.Join(fs.Args(), " "))
	}
	return nil
}
