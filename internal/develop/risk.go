package develop

import (
	"path"
	"regexp"
	"strings"
)

// dependencyFiles are manifests and lockfiles, by base name.
var dependencyFiles = regexp.MustCompile(`^(go\.mod|go\.sum|go\.work|go\.work\.sum|package\.json|package-lock\.json|` +
	`npm-shrinkwrap\.json|yarn\.lock|pnpm-lock\.yaml|bun\.lockb?|requirements.*\.txt|constraints.*\.txt|pyproject\.toml|` +
	`poetry\.lock|uv\.lock|Pipfile(\.lock)?|setup\.py|setup\.cfg|Cargo\.toml|Cargo\.lock|Gemfile(\.lock)?|.*\.gemspec|` +
	`composer\.(json|lock)|pom\.xml|build\.gradle(\.kts)?|settings\.gradle(\.kts)?|gradle\.lockfile|.*\.csproj|` +
	`packages\.lock\.json|Directory\.Packages\.props|mix\.(exs|lock)|Package\.swift|Package\.resolved|Podfile(\.lock)?)$`)

// authSegment matches a path segment about authentication or access
// control.
var authSegment = regexp.MustCompile(`(?i)auth|login|logout|session|passw|credential|secret|token|oauth|oidc|saml|jwt|` +
	`permission|rbac|acl|crypt|signin|signup`)

// Risky returns why changing these paths makes a PR high-risk, one reason
// per path that touches dependencies or authentication. It needs no
// agent and is applied whatever the pre-review says.
func Risky(paths []string) []string {
	var out []string
	for _, p := range paths {
		switch {
		case dependencyFiles.MatchString(path.Base(p)):
			out = append(out, "`"+p+"` is a dependency file")
		case anySegment(p):
			out = append(out, "`"+p+"` looks related to authentication or access control")
		}
	}
	return out
}

func anySegment(p string) bool {
	for _, seg := range strings.Split(p, "/") {
		if authSegment.MatchString(seg) {
			return true
		}
	}
	return false
}
