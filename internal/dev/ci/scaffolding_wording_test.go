//go:build dev

package ci

import (
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// scaffoldingSkill is the skill whose source-of-truth rule SCAF-SOT-01 the canon wording check reads.
const scaffoldingSkill = "plugins/crw/skills/crw-dev-scaffolding/"

var (
	scaffoldingRule   = regexp.MustCompile(`(?m)^\*\*Rule \(SCAF-SOT-01\):\*\*.*$`)
	scaffoldingDevlog = regexp.MustCompile(`(?i)devlog|\b_plan\b|\b_fin\b`)
)

// scaffoldingWordingErrors checks the wording of the scaffolding skill against the canon: product
// decisions live in Linear and the code structure in the repository's docs (SCAF-SOT-01), and the
// devlog/_plan model of implementation units is not described there any more.
func scaffoldingWordingErrors(root string) []string {
	var errs []string
	skill, err := os.ReadFile(filepath.Join(root, scaffoldingSkill, "SKILL.md"))
	if err != nil {
		return []string{err.Error()}
	}
	rule := scaffoldingRule.FindString(string(skill))
	switch {
	case rule == "":
		errs = append(errs, "SKILL.md: no line starting **Rule (SCAF-SOT-01):**")
	default:
		for _, want := range []string{"Linear", "product decision", "repo docs", "code structure"} {
			if !strings.Contains(rule, want) {
				errs = append(errs, "SKILL.md: the SCAF-SOT-01 rule does not say "+want+": "+rule)
			}
		}
	}
	files, err := dirTree{root}.list(scaffoldingSkill)
	if err != nil {
		return append(errs, err.Error())
	}
	for _, file := range files {
		data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(file)))
		if err != nil {
			errs = append(errs, err.Error())
			continue
		}
		for number, line := range lines(string(data)) {
			if scaffoldingDevlog.MatchString(line) {
				errs = append(errs, file+":"+strconv.Itoa(number+1)+": the devlog/_plan model is not part of the scaffolding skill: "+strings.TrimSpace(line))
			}
		}
	}
	return errs
}

func TestScaffoldingCanonWording(t *testing.T) {
	t.Run("the checked-in skill", func(t *testing.T) {
		if errs := scaffoldingWordingErrors(repoRoot()); len(errs) > 0 {
			t.Errorf("%s", strings.Join(errs, "\n"))
		}
	})
	fixture := func(t *testing.T, skill, reference string) string {
		t.Helper()
		root := t.TempDir()
		for name, text := range map[string]string{"SKILL.md": skill, "references/api-docs.md": reference} {
			target := filepath.Join(root, scaffoldingSkill, name)
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(target, []byte(text), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		return root
	}
	const rule = "**Rule (SCAF-SOT-01):** A product decision is canonical in Linear and the code structure in the repo docs.\n"
	t.Run("fixture: the canon wording passes", func(t *testing.T) {
		expectEqual(t, "errors", scaffoldingWordingErrors(fixture(t, "# S\n\n"+rule, "plain\n")), []string(nil))
	})
	t.Run("fixture: the old devlog rule fails", func(t *testing.T) {
		old := "**Rule (SCAF-SOT-01):** prefer the existing `devlog/` placement.\n"
		errs := scaffoldingWordingErrors(fixture(t, "# S\n\n"+old, "plain\n"))
		if len(errs) < 2 {
			t.Fatalf("errors = %v, want the missing wording and the devlog line", errs)
		}
	})
	t.Run("fixture: a devlog/_plan description anywhere in the skill fails", func(t *testing.T) {
		errs := scaffoldingWordingErrors(fixture(t, "# S\n\n"+rule, "a\nPlans live in devlog/_plan units.\n"))
		expectEqual(t, "errors", errs, []string{"plugins/crw/skills/crw-dev-scaffolding/references/api-docs.md:2: the devlog/_plan model is not part of the scaffolding skill: Plans live in devlog/_plan units."})
	})
	t.Run("fixture: a missing rule line fails", func(t *testing.T) {
		expectEqual(t, "errors", scaffoldingWordingErrors(fixture(t, "# S\n", "x\n")), []string{"SKILL.md: no line starting **Rule (SCAF-SOT-01):**"})
	})
}
