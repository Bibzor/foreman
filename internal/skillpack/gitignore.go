package skillpack

import (
	"bytes"
	"errors"
	"io/fs"
	"os"
	"strings"
)

const gitignore = ".gitignore"

// ensureGitignore appends each entry to the target's .gitignore unless a
// line already ignores it, with or without a leading or trailing slash.
func ensureGitignore(root *os.Root, entries ...string) error {
	b, err := root.ReadFile(gitignore)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	present := make(map[string]bool)
	for _, line := range strings.Split(string(b), "\n") {
		present[normalizeIgnore(line)] = true
	}
	var add []string
	for _, e := range entries {
		if !present[normalizeIgnore(e)] {
			add = append(add, e)
		}
	}
	if len(add) == 0 {
		return nil
	}
	if len(b) > 0 && !bytes.HasSuffix(b, []byte("\n")) {
		b = append(b, '\n')
	}
	for _, e := range add {
		b = append(b, e+"\n"...)
	}
	return root.WriteFile(gitignore, b, filePerm)
}

func normalizeIgnore(s string) string {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "/")
	return strings.TrimSuffix(s, "/")
}
