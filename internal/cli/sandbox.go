package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/Bibzor/foreman/internal/sandbox"
	sdocker "github.com/Bibzor/foreman/internal/sandbox/docker"
)

// sandboxRunner runs docker for `foreman sandbox exec`; a variable for
// tests.
var sandboxRunner sdocker.Runner = sdocker.Exec{}

// exitCode is an error that makes foreman exit with code: the status of
// the command run in the sandbox.
type exitCode int

func (e exitCode) Error() string { return fmt.Sprintf("exit status %d", int(e)) }

// sandboxExec runs one project command in the sandbox described by the
// spec in $FOREMAN_SANDBOX_SPEC, streaming its output. It is what the
// agent's Bash may run: the spec must be the user's own, mode 0600, under
// <data-dir>/skills, and the command one of the spec's.
func sandboxExec(args []string, stdout, stderr io.Writer) error {
	var dataDir string
	fs := flag.NewFlagSet("foreman sandbox exec", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.StringVar(&dataDir, "data-dir", "", "foreman's data directory (required)")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return err
		}
		return fmt.Errorf("%w: %w", errUsage, err)
	}
	if dataDir == "" || fs.NArg() != 1 {
		return fmt.Errorf("%w: sandbox exec needs --data-dir and exactly one command name", errUsage)
	}
	name := fs.Arg(0)
	s, err := sandbox.Read(os.Getenv(sandbox.EnvSpec), filepath.Clean(dataDir))
	if err != nil {
		return err
	}
	if _, ok := s.Command(name); !ok {
		return fmt.Errorf("%w: no sandbox command %q", errUsage, name)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	err = (&sdocker.Sandbox{Runner: sandboxRunner}).Exec(ctx, s, name, stdout)
	var ee *sandbox.ExitError
	if errors.As(err, &ee) {
		return exitCode(ee.Code)
	}
	return err
}
