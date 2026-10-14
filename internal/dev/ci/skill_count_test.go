//go:build dev

package ci

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

// maxSkillDirectories is the most skills the plugin holds: the 34 of the inventory base, less the
// three that were merged (CRW-1218 two, CRW-1220 one), plus the new skills of CRW-1216 to CRW-1220,
// which made 33, less crw-interview, which CRW-1340 removed (crw-define stays: Jun's 10-14 correction).
// A new skill is a net addition and has to take the place of one that goes away.
const maxSkillDirectories = 32

// skillDirectoryCountErrors counts the directories of root's plugins/crw/skills that hold a SKILL.md
// and reports the count when it is over limit.
func skillDirectoryCountErrors(root string, limit int) []string {
	entries, err := os.ReadDir(filepath.Join(root, "plugins/crw/skills"))
	if err != nil {
		return []string{err.Error()}
	}
	count := 0
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		if info, err := os.Stat(filepath.Join(root, "plugins/crw/skills", entry.Name(), "SKILL.md")); err == nil && info.Mode().IsRegular() {
			count++
		}
	}
	if count > limit {
		return []string{"plugins/crw/skills holds " + strconv.Itoa(count) + " directories with a SKILL.md, over " + strconv.Itoa(limit)}
	}
	return nil
}

func TestSkillDirectoryCountAtMost32(t *testing.T) {
	t.Run("the checked-in tree", func(t *testing.T) {
		if errs := skillDirectoryCountErrors(repoRoot(), maxSkillDirectories); len(errs) > 0 {
			t.Errorf("%v", errs)
		}
	})
	fixture := func(t *testing.T, skills, bare int) string {
		t.Helper()
		root := t.TempDir()
		for i := range skills {
			path := filepath.Join(root, "plugins/crw/skills", "crw-s"+strconv.Itoa(i), "SKILL.md")
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte("---\nname: s\n---\n"), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		for i := range bare {
			if err := os.MkdirAll(filepath.Join(root, "plugins/crw/skills", "bare-"+strconv.Itoa(i), "references"), 0o755); err != nil {
				t.Fatal(err)
			}
		}
		return root
	}
	t.Run("fixture: 32 skills pass, a 33rd fails", func(t *testing.T) {
		expectEqual(t, "32", skillDirectoryCountErrors(fixture(t, 32, 0), maxSkillDirectories), []string(nil))
		expectEqual(t, "33", skillDirectoryCountErrors(fixture(t, 33, 0), maxSkillDirectories),
			[]string{"plugins/crw/skills holds 33 directories with a SKILL.md, over 32"})
	})
	t.Run("fixture: a directory without a SKILL.md and a loose file are not skills", func(t *testing.T) {
		root := fixture(t, 32, 3)
		if err := os.WriteFile(filepath.Join(root, "plugins/crw/skills", "README.md"), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		expectEqual(t, "uncounted", skillDirectoryCountErrors(root, maxSkillDirectories), []string(nil))
	})
	t.Run("fixture: a missing skills directory is an error", func(t *testing.T) {
		if errs := skillDirectoryCountErrors(t.TempDir(), maxSkillDirectories); len(errs) != 1 {
			t.Errorf("errors = %v, want one", errs)
		}
	})
}
