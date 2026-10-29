// Package version holds the foreman binary version, set at build time with
// -ldflags "-X github.com/Bibzor/foreman/internal/version.Version=...".
// The skills pack has its own version, skills.PackVersion.
package version

// Version is the binary version.
var Version = "dev"
