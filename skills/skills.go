// Package skills embeds the skills pack shipped with foreman.
//
// The pack has its own version, PackVersion, independent of the binary
// version. It is bumped by hand in the pull request that changes a skill,
// together with PackSHA256: a test fails when the pack and its hash
// disagree, so a skill cannot change under an unchanged version.
package skills

import "embed"

// PackVersion is the version of the skills pack (semver). Projects pin it
// as skills_version in their .foreman.yml.
const PackVersion = "0.1.0"

// PackSHA256 is the hash of the pack at PackVersion, as computed by
// PackHash. Update it with PackVersion.
const PackSHA256 = "96fd5bee2126c75c3f5847fe23ccc61329c8d29e45714bf7a123956b212776be"

// FS holds every skill as <name>/SKILL.md.
//
//go:embed */SKILL.md
var FS embed.FS
