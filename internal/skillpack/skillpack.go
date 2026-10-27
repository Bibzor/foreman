// Package skillpack installs a skills pack into a repository and verifies
// an installed copy against the pack.
//
// The pack is an fs.FS laid out as <name>/SKILL.md (plus optional extra
// files per skill). In the binary it is the embedded skills directory, which
// is the only source of truth: the lock written into the target repository
// records what was installed but is never trusted by Verify.
package skillpack

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/Bibzor/foreman/internal/agent"
)

// ErrSymlink is returned when a path foreman writes or verifies is a symlink.
var ErrSymlink = errors.New("symlink")

const (
	dirPerm  = 0o755
	filePerm = 0o644
)

// Install writes every file of pack under the skills directory of the named
// agent in target, writes the lock and adds the installed directories to
// target's .gitignore. It refuses symlinks anywhere on the install path and
// never writes outside target. When it refuses, it writes nothing.
func Install(pack fs.FS, target, agentName, version string) (Lock, error) {
	dir, err := agent.SkillsDir(agentName)
	if err != nil {
		return Lock{}, err
	}
	if _, err := List(pack); err != nil {
		return Lock{}, err
	}
	files, err := packFiles(pack, dir)
	if err != nil {
		return Lock{}, err
	}

	// os.Root confines every operation to target: ".." and symlinks
	// pointing outside it fail instead of being followed.
	root, err := os.OpenRoot(target)
	if err != nil {
		return Lock{}, fmt.Errorf("open target: %w", err)
	}
	defer func() { _ = root.Close() }() // closing the directory handle cannot lose data

	stale := staleFiles(root, dir, files)

	checks := map[string]bool{ // path -> must be a directory
		lockDir:   true,
		lockPath:  false,
		lockTmp:   false,
		gitignore: false,
	}
	for p := range files {
		addWithParents(checks, p)
	}
	for _, p := range stale {
		addWithParents(checks, p)
	}
	if err := checkPaths(root, checks); err != nil {
		return Lock{}, fmt.Errorf("refusing to install: %w", err)
	}

	lock := Lock{Version: version, Agent: agentName, Files: make(map[string]string, len(files))}
	for _, p := range sortedKeys(files) {
		native := filepath.FromSlash(p)
		if err := root.MkdirAll(filepath.Dir(native), dirPerm); err != nil {
			return Lock{}, err
		}
		if err := root.WriteFile(native, files[p], filePerm); err != nil {
			return Lock{}, err
		}
		lock.Files[p] = hashBytes(files[p])
	}
	removeStale(root, dir, stale)
	if err := writeLock(root, lock); err != nil {
		return Lock{}, err
	}
	if err := ensureGitignore(root, dir+"/", lockDir+"/"); err != nil {
		return Lock{}, err
	}
	return lock, nil
}

// packFiles reads every file of pack, keyed by its slash-separated path
// under dir.
func packFiles(pack fs.FS, dir string) (map[string][]byte, error) {
	files := make(map[string][]byte)
	err := fs.WalkDir(pack, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		if !d.Type().IsRegular() {
			return fmt.Errorf("skills pack: %s is not a regular file", p)
		}
		b, err := fs.ReadFile(pack, p)
		if err != nil {
			return err
		}
		files[path.Join(dir, p)] = b
		return nil
	})
	if err != nil {
		return nil, err
	}
	if len(files) == 0 {
		return nil, errors.New("skills pack is empty")
	}
	return files, nil
}

// staleFiles returns the files a previous install recorded under dir that
// the pack no longer ships. The previous lock is untrusted, so entries
// outside dir are ignored.
func staleFiles(root *os.Root, dir string, files map[string][]byte) []string {
	prev, err := readLock(root)
	if err != nil {
		return nil
	}
	var stale []string
	for p := range prev.Files {
		if !filepath.IsLocal(p) || path.Clean(p) != p || !strings.HasPrefix(p, dir+"/") {
			continue
		}
		if _, ok := files[p]; !ok {
			stale = append(stale, p)
		}
	}
	sort.Strings(stale)
	return stale
}

// removeStale deletes stale files, then their directories once empty.
// Failures are ignored: a leftover file shows up as extra in Verify.
func removeStale(root *os.Root, dir string, stale []string) {
	for _, p := range stale {
		if root.Remove(filepath.FromSlash(p)) != nil {
			continue
		}
		for d := path.Dir(p); d != dir && strings.HasPrefix(d, dir+"/"); d = path.Dir(d) {
			if root.Remove(filepath.FromSlash(d)) != nil {
				break
			}
		}
	}
}

// addWithParents records p as a file and each of its parents as a directory.
func addWithParents(checks map[string]bool, p string) {
	checks[p] = false
	for d := path.Dir(p); d != "."; d = path.Dir(d) {
		checks[d] = true
	}
}

// checkPaths fails if any existing path is a symlink or is not the expected
// kind. Parents are checked before their children so that a symlinked
// directory is reported by its own name.
func checkPaths(root *os.Root, checks map[string]bool) error {
	paths := sortedKeys(checks)
	sort.SliceStable(paths, func(i, j int) bool {
		return strings.Count(paths[i], "/") < strings.Count(paths[j], "/")
	})
	for _, p := range paths {
		if err := checkPath(root, p, checks[p]); err != nil {
			return err
		}
	}
	return nil
}

func checkPath(root *os.Root, p string, wantDir bool) error {
	fi, err := root.Lstat(filepath.FromSlash(p))
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return nil
	case err != nil:
		return err
	case fi.Mode()&fs.ModeSymlink != 0:
		return fmt.Errorf("%s is a %w", p, ErrSymlink)
	case wantDir && !fi.IsDir():
		return fmt.Errorf("%s is not a directory", p)
	case !wantDir && !fi.Mode().IsRegular():
		return fmt.Errorf("%s is not a regular file", p)
	}
	return nil
}

func hashBytes(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
