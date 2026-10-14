package delivery

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A workflow restore tells the recipient which skill to read for each name in its skills list. The
// pointer must name skills plugins/crw/skills holds, with any reference file under the skill, and never
// the earlier codexclaw:cxc-* names, which named skills no shipped plugin has (CRW-389, CRW-195).
func TestRestorePointersNameShippedSkills(t *testing.T) {
	skills := filepath.Join(repoRoot(t), "plugins", "crw", "skills")
	for _, name := range []string{"development", "loop", "lost-context", "pull-request", "review-repair"} {
		var r map[string]any
		doc := `{"restore":{"mode":"direct","skills":["` + name + `"]}}`
		if err := json.Unmarshal([]byte(doc), &r); err != nil {
			t.Fatal(err)
		}
		var pointer string
		for _, line := range workRestoreLines(r) {
			if strings.HasPrefix(line, "  read: ") {
				pointer = strings.TrimPrefix(line, "  read: ")
			}
		}
		if pointer == "" {
			t.Errorf("%s: no read pointer in the restore lines", name)
			continue
		}
		if strings.Contains(pointer, "codexclaw") {
			t.Errorf("%s: pointer %q still carries the CXC name", name, pointer)
		}
		current, named := "", 0
		for _, tok := range strings.Fields(pointer) {
			switch {
			case strings.HasPrefix(tok, "crw:crw-"):
				current = strings.TrimPrefix(tok, "crw:")
				named++
				if _, err := os.Stat(filepath.Join(skills, current, "SKILL.md")); err != nil {
					t.Errorf("%s: pointer %q names %s, which plugins/crw/skills does not hold", name, pointer, current)
				}
			case strings.HasPrefix(tok, "references/"):
				if current == "" {
					t.Errorf("%s: pointer %q names a reference before any skill", name, pointer)
					continue
				}
				if _, err := os.Stat(filepath.Join(skills, current, tok)); err != nil {
					t.Errorf("%s: pointer %q names %s under %s, which does not exist", name, pointer, tok, current)
				}
			case tok == "with":
			default:
				t.Errorf("%s: pointer %q has the token %q, which is not a crw skill or a reference", name, pointer, tok)
			}
		}
		if named == 0 {
			t.Errorf("%s: pointer %q names no crw skill", name, pointer)
		}
	}
}
