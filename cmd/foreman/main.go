// Command foreman orchestrates AI agents over GitHub issues.
package main

import (
	"os"

	"github.com/Bibzor/foreman/internal/cli"
)

func main() {
	os.Exit(cli.Run(os.Args[1:], os.Stdout, os.Stderr))
}
