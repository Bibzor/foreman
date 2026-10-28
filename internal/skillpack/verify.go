package skillpack

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"

	"github.com/Bibzor/foreman/internal/agent"
)

// Report is the result of Verify. Paths are slash-separated, relative to
// the target, and sorted.
type Report struct {
	BinaryVersion string
	LockVersion   string
	Missing       []string // in the pack, absent from the target
	Modified      []string // content differs from the pack, or not a regular file
	Extra         []string // in the skills directory, not in the pack
}

// OK reports whether the installed skills match the pack exactly.
func (r Report) OK() bool {
	return r.LockVersion == r.BinaryVersion && len(r.Missing)+len(r.Modified)+len(r.Extra) == 0
}

// Verify compares the skills installed in target with pack, the source of
// truth. The lock is read only for the installed version and agent: its
// hashes are never used, since anything that can modify a skill in the
// target can also rewrite the lock.
func Verify(pack fs.FS, target, version string) (Report, error) {
	root, err := os.OpenRoot(target)
	if err != nil {
		return Report{}, fmt.Errorf("open target: %w", err)
	}
	defer func() { _ = root.Close() }() // closing the directory handle cannot lose data

	if err := checkPaths(root, map[string]bool{lockDir: true, lockPath: false}); err != nil {
		return Report{}, err
	}
	lock, err := readLock(root)
	if err != nil {
		return Report{}, err
	}
	dir, err := agent.SkillsDir(lock.Agent)
	if err != nil {
		return Report{}, fmt.Errorf("%s: %w", lockPath, err)
	}
	checks := make(map[string]bool)
	addWithParents(checks, dir)
	checks[dir] = true
	if err := checkPaths(root, checks); err != nil {
		return Report{}, err
	}
	files, err := packFiles(pack, dir)
	if err != nil {
		return Report{}, err
	}

	r := Report{BinaryVersion: version, LockVersion: lock.Version}
	for _, p := range sortedKeys(files) {
		switch state(root, p, files[p]) {
		case missing:
			r.Missing = append(r.Missing, p)
		case modified:
			r.Modified = append(r.Modified, p)
		}
	}

	// WalkDir does not follow symlinks: a link is reported as an entry and
	// never descended into.
	err = fs.WalkDir(root.FS(), dir, func(p string, d fs.DirEntry, err error) error {
		if p == dir && errors.Is(err, fs.ErrNotExist) {
			return fs.SkipAll
		}
		if err != nil || d.IsDir() {
			return err
		}
		if _, ok := files[p]; !ok {
			r.Extra = append(r.Extra, p)
		}
		return nil
	})
	if err != nil {
		return Report{}, err
	}
	sort.Strings(r.Extra)
	return r, nil
}

type fileState int

const (
	same fileState = iota
	missing
	modified
)

func state(root *os.Root, p string, want []byte) fileState {
	native := filepath.FromSlash(p)
	fi, err := root.Lstat(native)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		// A missing parent skill directory also lands here.
		if _, perr := root.Lstat(filepath.FromSlash(path.Dir(p))); perr == nil || errors.Is(perr, fs.ErrNotExist) {
			return missing
		}
		return modified
	case err != nil, !fi.Mode().IsRegular():
		// Includes a symlinked file, or a parent replaced by a symlink.
		return modified
	}
	got, err := root.ReadFile(native)
	if err != nil || hashBytes(got) != hashBytes(want) {
		return modified
	}
	return same
}
