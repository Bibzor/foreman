package skillpack

import (
	"errors"
	"fmt"
	"io/fs"
	"strings"
)

// Skill is a skill's front matter.
type Skill struct {
	Name        string
	Description string
}

// List returns the skills of pack sorted by name. Every top-level directory
// must hold a SKILL.md whose front matter has a name equal to the directory
// and a non-empty description.
func List(pack fs.FS) ([]Skill, error) {
	entries, err := fs.ReadDir(pack, ".")
	if err != nil {
		return nil, err
	}
	var skills []Skill
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		p := e.Name() + "/SKILL.md"
		b, err := fs.ReadFile(pack, p)
		if err != nil {
			return nil, err
		}
		fm, err := parseFrontMatter(string(b))
		if err != nil {
			return nil, fmt.Errorf("%s: %w", p, err)
		}
		s := Skill{Name: fm["name"], Description: fm["description"]}
		if s.Name != e.Name() {
			return nil, fmt.Errorf("%s: name %q does not match directory %q", p, s.Name, e.Name())
		}
		if s.Description == "" {
			return nil, fmt.Errorf("%s: missing description", p)
		}
		skills = append(skills, s)
	}
	return skills, nil // fs.ReadDir returns entries sorted by name
}

// parseFrontMatter reads the "key: value" lines between the leading "---"
// delimiters. That is all the skills use, so no YAML parser is needed.
func parseFrontMatter(s string) (map[string]string, error) {
	lines := strings.Split(s, "\n")
	if strings.TrimRight(lines[0], "\r") != "---" {
		return nil, errors.New("missing front matter")
	}
	fm := make(map[string]string)
	for _, line := range lines[1:] {
		line = strings.TrimRight(line, "\r")
		if line == "---" {
			return fm, nil
		}
		if k, v, ok := strings.Cut(line, ":"); ok {
			fm[strings.TrimSpace(k)] = strings.TrimSpace(v)
		}
	}
	return nil, errors.New("unterminated front matter")
}
