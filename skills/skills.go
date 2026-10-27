// Package skills embeds the skills pack shipped with foreman. Its version
// is the binary version.
package skills

import "embed"

// FS holds every skill as <name>/SKILL.md.
//
//go:embed */SKILL.md
var FS embed.FS
