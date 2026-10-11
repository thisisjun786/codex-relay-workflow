//go:build dev

package ci

import (
	"crypto/sha256"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// duplicateAssetsSkillsRoot is the tree the done rows' paths lie in; its first path element below is
// the skill a file belongs to.
const duplicateAssetsSkillsRoot = "plugins/crw/skills/"

// perSkillFiles are the file names a skill or one of its directories carries for itself (the skill
// file, its interface and the README that describes a directory), so the same name in two skills is
// no duplicate.
var perSkillFiles = []string{"SKILL.md", "openai.yaml", "README.md"}

// duplicateAssetErrors reads only the paths of the done rows whose check names TestNoDuplicateSkillAssets and
// reports each file name that two skills among them both carry: one copy of an asset lives in one
// skill and the others link to it. A copy that differs from the other is named as diverged. A done
// row without paths, or whose paths hold no file, is an error: a done row cannot pass by reading nothing.
func duplicateAssetErrors(root string, m inventoryMap) []string {
	var errs []string
	for _, key := range sortedKeys(m.Rows) {
		row := m.Rows[key]
		named := false
		for _, name := range row.Check {
			named = named || name == "TestNoDuplicateSkillAssets"
		}
		if row.Status != "done" || !named {
			continue
		}
		if len(row.Paths) == 0 {
			errs = append(errs, key+": a done row that names TestNoDuplicateSkillAssets has no paths to read")
		}
		type copyOf struct{ skill, file, digest string }
		byName := map[string][]copyOf{}
		seen := map[string]bool{}
		for _, p := range row.Paths {
			files, err := dirTree{root}.list(p)
			if err != nil {
				errs = append(errs, key+": "+err.Error())
				continue
			}
			if len(files) == 0 {
				errs = append(errs, key+": "+p+" holds no file to compare")
			}
			for _, file := range files {
				rest, ok := strings.CutPrefix(file, duplicateAssetsSkillsRoot)
				skill, _, _ := strings.Cut(rest, "/")
				if !ok || skill == "" || seen[file] {
					continue
				}
				seen[file] = true
				base := path.Base(file)
				if slices.Contains(perSkillFiles, base) {
					continue
				}
				data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(file)))
				if err != nil {
					errs = append(errs, key+": "+err.Error())
					continue
				}
				sum := sha256.Sum256(data)
				byName[base] = append(byName[base], copyOf{skill, file, string(sum[:])})
			}
		}
		for _, name := range sortedKeys(byName) {
			copies := byName[name]
			for i := range copies {
				for j := i + 1; j < len(copies); j++ {
					if copies[i].skill == copies[j].skill {
						continue
					}
					kind := "a copy of"
					if copies[i].digest != copies[j].digest {
						kind = "a diverged copy of"
					}
					errs = append(errs, key+": "+copies[j].file+" is "+kind+" "+copies[i].file)
				}
			}
		}
	}
	return errs
}

func duplicateFixture(t *testing.T, files map[string]string, status string, paths ...string) (string, inventoryMap) {
	t.Helper()
	root := t.TempDir()
	for name, text := range files {
		target := filepath.Join(root, duplicateAssetsSkillsRoot, name)
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(target, []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	row := inventoryRow{Disposition: "fix", Owner: "S06", Check: []string{"TestNoDuplicateSkillAssets"}, Status: status, Paths: paths}
	return root, inventoryMap{Rows: map[string]inventoryRow{"duplicateContent[9]": row}}
}

func TestNoDuplicateSkillAssets(t *testing.T) {
	t.Run("the checked-in table", func(t *testing.T) {
		root := repoRoot()
		data, err := os.ReadFile(filepath.Join(root, inventoryMapPath))
		if err != nil {
			t.Fatal(err)
		}
		m, err := loadInventoryMap(data)
		if err != nil {
			t.Fatal(err)
		}
		if errs := duplicateAssetErrors(root, m); len(errs) > 0 {
			t.Errorf("%s", strings.Join(errs, "\n"))
		}
	})
	files := map[string]string{
		"crw-a/SKILL.md":            "a",
		"crw-b/SKILL.md":            "b",
		"crw-a/scripts/audit.sh":    "one",
		"crw-b/scripts/audit.sh":    "two",
		"crw-a/references/same.md":  "same",
		"crw-b/references/same.md":  "same",
		"crw-a/agents/openai.yaml":  "x",
		"crw-b/agents/openai.yaml":  "y",
		"crw-a/references/inner.md": "p",
		"crw-a/other/inner.md":      "q",
		"crw-c/references/solo.md":  "solo",
	}
	t.Run("fixture: a same-named file in two skills fails, diverged or not", func(t *testing.T) {
		root, m := duplicateFixture(t, files, "done", duplicateAssetsSkillsRoot+"crw-a/", duplicateAssetsSkillsRoot+"crw-b/")
		expectEqual(t, "errors", duplicateAssetErrors(root, m), []string{
			"duplicateContent[9]: plugins/crw/skills/crw-b/scripts/audit.sh is a diverged copy of plugins/crw/skills/crw-a/scripts/audit.sh",
			"duplicateContent[9]: plugins/crw/skills/crw-b/references/same.md is a copy of plugins/crw/skills/crw-a/references/same.md",
		})
	})
	t.Run("fixture: the same name twice in one skill, a skill outside the paths and the per-skill files are not duplicates", func(t *testing.T) {
		root, m := duplicateFixture(t, files, "done", duplicateAssetsSkillsRoot+"crw-a/", duplicateAssetsSkillsRoot+"crw-c/")
		expectEqual(t, "errors", duplicateAssetErrors(root, m), []string(nil))
	})
	t.Run("fixture: an open row is not read", func(t *testing.T) {
		root, m := duplicateFixture(t, files, "open", duplicateAssetsSkillsRoot+"crw-a/", duplicateAssetsSkillsRoot+"crw-b/")
		expectEqual(t, "errors", duplicateAssetErrors(root, m), []string(nil))
	})
	t.Run("fixture: a done row cannot pass by reading nothing", func(t *testing.T) {
		root, m := duplicateFixture(t, files, "done")
		expectEqual(t, "no paths", duplicateAssetErrors(root, m), []string{"duplicateContent[9]: a done row that names TestNoDuplicateSkillAssets has no paths to read"})
		root, m = duplicateFixture(t, files, "done", duplicateAssetsSkillsRoot+"crw-none/")
		expectEqual(t, "empty path", duplicateAssetErrors(root, m), []string{"duplicateContent[9]: plugins/crw/skills/crw-none/ holds no file to compare"})
	})
}

// The visualizer's host-freshness helper sits outside the skill (docs/port-cxc/visualizer), so the
// skill-scripts-node collector (plugins/crw/skills/*/tests) does not reach its test. This test is
// the verification entry point that runs it with the rest of internal/dev/ci.
func TestVisualizerSyncCheckHelperTestRuns(t *testing.T) {
	const helperTest = "docs/port-cxc/visualizer/sync-check.test.mjs"
	if _, err := os.Stat(filepath.Join(repoRoot(), helperTest)); err != nil {
		t.Fatal(err)
	}
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("no node")
	}
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("no bash")
	}
	got := runCommand(t, repoRoot(), nil, node, "--test", helperTest)
	if got.code != 0 {
		t.Errorf("node --test %s: exit %d\n%s%s", helperTest, got.code, got.stdout, got.stderr)
	}
}
