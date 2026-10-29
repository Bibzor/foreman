package project

import (
	"errors"
	"path"
	"strings"
)

// validGlob checks a test_paths pattern: slash-separated, relative, each
// segment either "**" or a path.Match pattern.
func validGlob(g string) error {
	switch {
	case g == "":
		return errors.New("empty pattern")
	case strings.HasPrefix(g, "/"):
		return errors.New("must be relative")
	case strings.ContainsAny(g, "\\ \t\n\r"):
		return errors.New("no backslash or whitespace")
	}
	for _, seg := range strings.Split(g, "/") {
		switch {
		case seg == "":
			return errors.New("empty path segment")
		case seg == "." || seg == "..":
			return errors.New(`no "." or ".." segment`)
		case seg == "**":
			continue
		case strings.Contains(seg, "**"):
			return errors.New(`"**" must be a whole path segment`)
		}
		if _, err := path.Match(seg, ""); err != nil {
			return err
		}
	}
	return nil
}

// Match reports whether the slash-separated path p matches the pattern g.
// A "**" segment matches zero or more whole segments; any other segment
// matches one segment as path.Match does. g must have passed validGlob.
func Match(g, p string) bool {
	return match(strings.Split(g, "/"), strings.Split(p, "/"))
}

func match(pat, segs []string) bool {
	for len(pat) > 0 {
		if pat[0] == "**" {
			for i := 0; i <= len(segs); i++ {
				if match(pat[1:], segs[i:]) {
					return true
				}
			}
			return false
		}
		if len(segs) == 0 {
			return false
		}
		if ok, err := path.Match(pat[0], segs[0]); err != nil || !ok {
			return false
		}
		pat, segs = pat[1:], segs[1:]
	}
	return len(segs) == 0
}
