package cli

import sdocker "github.com/Bibzor/foreman/internal/sandbox/docker"

// SetSandboxRunner makes `foreman sandbox exec` run docker through r,
// until the returned func restores it.
func SetSandboxRunner(r sdocker.Runner) (restore func()) {
	old := sandboxRunner
	sandboxRunner = r
	return func() { sandboxRunner = old }
}
