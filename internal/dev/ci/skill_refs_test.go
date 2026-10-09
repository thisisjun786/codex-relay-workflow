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

// Every crw:crw-<name> a shipped skill names resolves to a skill the plugin ships under
// plugins/crw/skills, where the activation move (CRW-392) put the ported skills too. A reference that
// resolves to none would be a skill the plugin tells a child to load and nothing provides.
func TestShippedSkillsNameOnlySkillsTheRepositoryHolds(t *testing.T) {
	root := repoRoot()
	held := map[string]string{}
	for _, dir := range []string{"plugins/crw/skills"} {
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
				t.Errorf("%s names crw:%s, a skill plugins/crw/skills does not hold", rel, m[1])
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
