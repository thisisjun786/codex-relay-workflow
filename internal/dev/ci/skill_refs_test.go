//go:build dev

package ci

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// Every crw:crw-<name> a shipped skill names resolves to a skill this repository holds, either shipped
// under plugins/crw/skills or staged under port/cxc/skills for the activation move to ship with it. A
// reference that resolves to neither would be a skill the plugin tells a child to load and nothing
// provides, so the move cannot leave one behind.
func TestShippedSkillsNameOnlySkillsTheRepositoryHolds(t *testing.T) {
	root := repoRoot()
	held := map[string]string{}
	for _, dir := range []string{"plugins/crw/skills", "port/cxc/skills"} {
		entries, err := os.ReadDir(filepath.Join(root, dir))
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range entries {
			if _, err := os.Stat(filepath.Join(root, dir, e.Name(), "SKILL.md")); err == nil {
				held[e.Name()] = dir
			}
		}
	}
	ref := regexp.MustCompile(`crw:(crw-[a-z0-9]+(?:-[a-z0-9]+)*)`)
	seen := 0
	err := filepath.WalkDir(filepath.Join(root, "plugins/crw/skills"), func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".md") {
			return err
		}
		text, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for _, m := range ref.FindAllStringSubmatch(string(text), -1) {
			seen++
			if _, ok := held[m[1]]; !ok {
				rel, _ := filepath.Rel(root, path)
				t.Errorf("%s names crw:%s, a skill neither plugins/crw/skills nor port/cxc/skills holds", rel, m[1])
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if seen == 0 {
		t.Error("no crw:crw-* reference found: the pattern or the skills directory moved")
	}
}
