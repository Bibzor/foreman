// Package version holds the foreman version, set at build time with
// -ldflags "-X github.com/Bibzor/foreman/internal/version.Version=...".
// The embedded skills pack carries the same version.
package version

// Version is the binary and skills pack version.
var Version = "dev"
