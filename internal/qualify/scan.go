package qualify

import (
	"fmt"
	"regexp"
	"strings"
	"unicode"
)

// Finding is a deterministic check hit. Any finding makes the issue
// suspicious, whatever the agent says.
type Finding struct {
	Where  string // "title", "body", "comment 123 by alice"
	Reason string
}

func (f Finding) String() string { return f.Where + ": " + f.Reason }

// hidden reports characters that render as nothing or reorder text:
// zero-width, bidi controls and Unicode tag characters (which can carry
// invisible ASCII).
func hidden(r rune) bool {
	switch r {
	case 0x200B, 0x200C, 0x200D, 0x2060, 0xFEFF, 0x180E, // zero-width
		0x200E, 0x200F, 0x061C, // marks
		0x202A, 0x202B, 0x202C, 0x202D, 0x202E, // embeddings and overrides
		0x2066, 0x2067, 0x2068, 0x2069: // isolates
		return true
	}
	return r >= 0xE0000 && r <= 0xE007F
}

// invisible is wider than hidden: every format character is made visible
// in what the agent and the reviewers read.
func invisible(r rune) bool { return hidden(r) || unicode.Is(unicode.Cf, r) }

var (
	b64Run  = regexp.MustCompile(`[A-Za-z0-9+/_-]{60,}={0,2}`)
	tagName = regexp.MustCompile(`(?i)untrusted_(issue|review|diff|pr_body|commits|feedback)`)
	tagLike = regexp.MustCompile(`(?i)<[\s/]*untrusted_(issue|review|diff|pr_body|commits|feedback)[^<>]*>`)
)

// minB64Run is the shortest piece between slashes counted as encoded.
const minB64Run = 32

// Scan runs the deterministic checks on every part of the snapshot.
func Scan(s Snapshot) []Finding {
	var out []Finding
	out = append(out, scanText("title", s.Title)...)
	out = append(out, scanText("body", s.Body)...)
	for _, c := range s.Comments {
		out = append(out, scanText(fmt.Sprintf("comment %d by %s", c.ID, c.Author), c.Body)...)
	}
	return out
}

func scanText(where, s string) []Finding {
	var out []Finding
	add := func(format string, args ...any) {
		out = append(out, Finding{Where: where, Reason: fmt.Sprintf(format, args...)})
	}
	if strings.Contains(s, "<!--") {
		add("contains an HTML comment")
	}
	seen := map[rune]bool{}
	for _, r := range s {
		if hidden(r) && !seen[r] {
			seen[r] = true
			add("contains the hidden character U+%04X", r)
		}
	}
	for _, m := range b64Run.FindAllString(s, -1) {
		if looksEncoded(m) {
			add("contains a %d-character base64-looking blob", len(m))
			break
		}
	}
	if tagName.MatchString(strings.Map(dropInvisible, s)) {
		add("mentions the untrusted_issue delimiter")
	}
	return out
}

// looksEncoded tells a base64 blob from long identifiers and paths: some
// piece between slashes is at least 32 characters and mixes upper case,
// lower case and digits. Hex hashes (no upper case) and URL paths made of
// words do not match.
func looksEncoded(run string) bool {
	for _, piece := range strings.Split(strings.TrimRight(run, "="), "/") {
		if len(piece) < minB64Run {
			continue
		}
		var upper, lower, digit bool
		for _, r := range piece {
			switch {
			case r >= 'A' && r <= 'Z':
				upper = true
			case r >= 'a' && r <= 'z':
				lower = true
			case r >= '0' && r <= '9':
				digit = true
			}
		}
		if upper && lower && digit {
			return true
		}
	}
	return false
}

func dropInvisible(r rune) rune {
	if invisible(r) {
		return -1
	}
	return r
}

// MakeVisible replaces every invisible character with its <U+XXXX> code.
func MakeVisible(s string) string {
	var b strings.Builder
	for _, r := range s {
		if invisible(r) {
			fmt.Fprintf(&b, "<U+%04X>", r)
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

// Neutralize makes sure s cannot open or close an <untrusted_issue>,
// <untrusted_review>, <untrusted_diff>, <untrusted_pr_body>,
// <untrusted_commits> or <untrusted_feedback> block: tag-like sequences are
// replaced, then any remaining occurrence of a name, in any case, is
// renamed. Run it after MakeVisible, so a
// name split by invisible characters is already broken up visibly.
func Neutralize(s string) string {
	s = tagLike.ReplaceAllStringFunc(s, func(m string) string {
		return "[removed tag: untrusted-" + strings.ToLower(tagName.FindStringSubmatch(m)[1]) + "]"
	})
	return tagName.ReplaceAllStringFunc(s, func(m string) string {
		return "untrusted-" + strings.ToLower(tagName.FindStringSubmatch(m)[1])
	})
}

// Visible is what agents and reviewers read of issue text: invisible
// characters shown, delimiter neutralized.
func Visible(s string) string { return Neutralize(MakeVisible(s)) }
