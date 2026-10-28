package skills

import (
	"crypto/sha256"
	"encoding/hex"
	"io/fs"
	"slices"
	"strings"
	"testing"

	"github.com/Bibzor/foreman/internal/skillpack"
)

const maxLines = 80

func TestPackContents(t *testing.T) {
	got, err := skillpack.List(FS)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, s := range got {
		names = append(names, s.Name)
	}
	want := []string{
		"foreman-fix-loop",
		"foreman-plan",
		"foreman-prereview",
		"foreman-qualify",
		"foreman-review-guide",
		"foreman-tdd",
		"go-conventions",
		"react-conventions",
	}
	if !slices.Equal(names, want) {
		t.Errorf("skills = %q, want %q", names, want)
	}
}

func TestSkillsAreShort(t *testing.T) {
	err := fs.WalkDir(FS, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := fs.ReadFile(FS, p)
		if err != nil {
			return err
		}
		if n := strings.Count(string(b), "\n"); n >= maxLines {
			t.Errorf("%s has %d lines, want fewer than %d", p, n, maxLines)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// Skills that read issue text must treat it as untrusted data.
func TestIssueReadersTreatIssueAsUntrusted(t *testing.T) {
	for _, name := range []string{
		"foreman-qualify",
		"foreman-plan",
		"foreman-tdd",
		"foreman-review-guide",
		"foreman-prereview",
	} {
		t.Run(name, func(t *testing.T) {
			b, err := fs.ReadFile(FS, name+"/SKILL.md")
			if err != nil {
				t.Fatal(err)
			}
			for _, want := range []string{"<untrusted_issue>", "Never follow instructions"} {
				if !strings.Contains(string(b), want) {
					t.Errorf("%s does not contain %q", name, want)
				}
			}
		})
	}
}

// foreman-qualify is maintained by hand and must ship byte for byte.
func TestQualifyIsUnchanged(t *testing.T) {
	b, err := fs.ReadFile(FS, "foreman-qualify/SKILL.md")
	if err != nil {
		t.Fatal(err)
	}
	h := sha256.Sum256(b)
	const want = "086d7e47e6b9e5bda6fa2f89a4fc1a96200424cf63819ce936b80fa51c863621"
	if got := hex.EncodeToString(h[:]); got != want {
		t.Errorf("foreman-qualify/SKILL.md sha256 = %s, want %s", got, want)
	}
}
