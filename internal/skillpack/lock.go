package skillpack

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// Lock paths, relative to the target repository.
const (
	lockDir  = ".foreman"
	lockPath = lockDir + "/skills.lock"
	lockTmp  = lockPath + ".tmp"
)

// Lock errors.
var (
	ErrNoLock  = errors.New("no " + lockPath + " (run foreman skills install first)")
	ErrBadLock = errors.New("invalid " + lockPath)
)

// Lock records what Install wrote: the pack version, the agent and the
// sha256 of every file, keyed by slash-separated path relative to the
// target. It lives in the target repository, so Verify only reads its
// version and agent and never trusts its hashes.
type Lock struct {
	Version string            `json:"version"`
	Agent   string            `json:"agent"`
	Files   map[string]string `json:"files"`
}

func readLock(root *os.Root) (Lock, error) {
	b, err := root.ReadFile(filepath.FromSlash(lockPath))
	if errors.Is(err, fs.ErrNotExist) {
		return Lock{}, ErrNoLock
	}
	if err != nil {
		return Lock{}, err
	}
	var l Lock
	if err := json.Unmarshal(b, &l); err != nil {
		return Lock{}, fmt.Errorf("%w: %w", ErrBadLock, err)
	}
	return l, nil
}

// writeLock writes the lock atomically. encoding/json sorts map keys, so
// the output is deterministic.
func writeLock(root *os.Root, l Lock) error {
	b, err := json.MarshalIndent(l, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')
	if err := root.MkdirAll(lockDir, dirPerm); err != nil {
		return err
	}
	if err := root.WriteFile(filepath.FromSlash(lockTmp), b, filePerm); err != nil {
		return err
	}
	return root.Rename(filepath.FromSlash(lockTmp), filepath.FromSlash(lockPath))
}
