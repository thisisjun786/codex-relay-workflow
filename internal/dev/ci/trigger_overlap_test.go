//go:build dev

package ci

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// triggerSkillsRoot is the skills directory the trigger table reads the descriptions of.
const triggerSkillsRoot = "plugins/crw/skills/"

// skillsFixture is a checkout in a temporary directory with one skill per entry of descriptions
// (skill directory name to the description in its SKILL.md frontmatter).
func skillsFixture(t *testing.T, descriptions map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for name, description := range descriptions {
		quoted, err := json.Marshal(description)
		if err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(root, triggerSkillsRoot, name, "SKILL.md")
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("---\nname: "+name+"\ndescription: "+string(quoted)+"\n---\n\n# "+name+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

// triggerOverlapErrors is the trigger table of the done rows checked against the frontmatter
// descriptions of root's plugins/crw skills: each phrase (case-insensitive) appears in the
// description of its owning skill and in no other (none: in no description), and a done row whose
// check names TestTriggerOverlapTable carries triggers.
func triggerOverlapErrors(root string, m inventoryMap) []string {
	descriptions, err := skillDescriptions(root)
	if err != nil {
		return []string{err.Error()}
	}
	var errs []string
	for _, key := range sortedKeys(m.Rows) {
		row := m.Rows[key]
		if row.Status != "done" {
			continue
		}
		if len(row.Triggers) == 0 {
			for _, name := range row.Check {
				if name == "TestTriggerOverlapTable" {
					errs = append(errs, key+": a done row that names TestTriggerOverlapTable has no triggers")
				}
			}
		}
		for _, phrase := range sortedKeys(row.Triggers) {
			owner := row.Triggers[phrase]
			var holders []string
			for _, skill := range sortedKeys(descriptions) {
				if strings.Contains(strings.ToLower(descriptions[skill]), strings.ToLower(phrase)) {
					holders = append(holders, skill)
				}
			}
			switch {
			case owner == "none" && len(holders) > 0:
				errs = append(errs, key+": trigger "+phrase+" must be in no description, found in "+strings.Join(holders, ", "))
			case owner == "none":
			default:
				if _, ok := descriptions[owner]; !ok {
					errs = append(errs, key+": trigger "+phrase+" names the skill "+owner+", which has no SKILL.md")
				} else if len(holders) != 1 || holders[0] != owner {
					errs = append(errs, key+": trigger "+phrase+" must be in the description of "+owner+" only, found in ["+strings.Join(holders, ", ")+"]")
				}
			}
		}
	}
	return errs
}

func TestTriggerOverlapTable(t *testing.T) {
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
		if errs := triggerOverlapErrors(root, m); len(errs) > 0 {
			t.Errorf("%s", strings.Join(errs, "\n"))
		}
	})
	root := skillsFixture(t, map[string]string{
		"crw-plan": "Decompose a goal into tracking issues. Use for a roadmap or 기획.",
		"crw-work": "Run the task work loop on one task.",
		"crw-ask":  "Ask clarifying questions. Use for 요구사항 정리.",
		"crw-qa":   "Manual QA of a surface; also mentions the Roadmap by mistake.",
	})
	done := func(check string, triggers map[string]string) inventoryRow {
		return inventoryRow{Disposition: "fix", Owner: "S04", Check: []string{check}, Status: "done", Triggers: triggers}
	}
	run := func(row inventoryRow) []string {
		return triggerOverlapErrors(root, inventoryMap{Rows: map[string]inventoryRow{"collisions[1]": row}})
	}
	t.Run("fixture: phrases in their owner's description only", func(t *testing.T) {
		expectEqual(t, "ok", run(done("TestTriggerOverlapTable", map[string]string{"기획": "crw-plan", "요구사항 정리": "crw-ask", "plan this": "none"})), []string(nil))
	})
	t.Run("fixture: a phrase that two descriptions hold fails, whatever the case", func(t *testing.T) {
		errs := run(done("TestTriggerOverlapTable", map[string]string{"roadmap": "crw-plan"}))
		expectEqual(t, "errors", errs, []string{"collisions[1]: trigger roadmap must be in the description of crw-plan only, found in [crw-plan, crw-qa]"})
	})
	t.Run("fixture: a phrase the owner lacks, a none phrase that is held and an unknown owner fail", func(t *testing.T) {
		errs := run(done("TestTriggerOverlapTable", map[string]string{"clarifying": "crw-plan", "기획": "none", "plan": "crw-missing"}))
		expectEqual(t, "errors", errs, []string{
			"collisions[1]: trigger clarifying must be in the description of crw-plan only, found in [crw-ask]",
			"collisions[1]: trigger plan names the skill crw-missing, which has no SKILL.md",
			"collisions[1]: trigger 기획 must be in no description, found in crw-plan",
		})
	})
	t.Run("fixture: a done row that names the test with empty triggers fails", func(t *testing.T) {
		expectEqual(t, "errors", run(done("TestTriggerOverlapTable", nil)), []string{"collisions[1]: a done row that names TestTriggerOverlapTable has no triggers"})
		expectEqual(t, "other test", run(done("TestSomethingElse", nil)), []string(nil))
	})
	t.Run("fixture: an open row is not read", func(t *testing.T) {
		row := done("TestTriggerOverlapTable", map[string]string{"roadmap": "crw-plan"})
		row.Status = "open"
		expectEqual(t, "open", run(row), []string(nil))
	})
}
