package skills

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"sort"
)

// PackHash returns the sha256 of every file of pack: for each file, sorted
// by path, its path, a NUL, its length, a NUL and its bytes.
func PackHash(pack fs.FS) (string, error) {
	var paths []string
	err := fs.WalkDir(pack, ".", func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			paths = append(paths, p)
		}
		return err
	})
	if err != nil {
		return "", err
	}
	sort.Strings(paths)
	h := sha256.New()
	for _, p := range paths {
		b, err := fs.ReadFile(pack, p)
		if err != nil {
			return "", err
		}
		_, _ = fmt.Fprintf(h, "%s\x00%d\x00", p, len(b)) // a hash never fails to write
		_, _ = h.Write(b)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
