//go:build dev

package ci

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/dev/skillport"
)

// stagedRepo is validateRepo plus one skill, whose original text is skill, staged by the staging
// tool from a synthetic CXC tree (a scratch repository stands in for the extracted tree).
func stagedRepo(t *testing.T, skill string) *fixtureRepo {
	t.Helper()
	r, tree := validateRepo(t), newRepo(t)
	table, err := os.ReadFile(filepath.Join(repoRoot(), "contract/schema/cxc/name-substitution.json"))
	if err != nil {
		t.Fatal(err)
	}
	r.write("contract/schema/cxc/name-substitution.json", string(table))
	tree.write("plugins/codexclaw/skills/kwrite/SKILL.md", skill)
	tree.write("plugins/codexclaw/skills/kwrite/agents/openai.yaml", "interface:\n  display_name: \"cxc-kwrite\"\n  short_description: \"Demo\"\n  default_prompt: \"$cxc-kwrite demo\"\n")
	tree.write("plugins/codexclaw/skills/kwrite/references/a.md", "# A\n")
	listing, err := skillport.Listing(filepath.Join(tree.root, "plugins/codexclaw/skills"))
	if err != nil {
		t.Fatal(err)
	}
	src := skillport.Source{Dir: tree.root, Origin: skillport.Origin{Tag: "test", Commit: "c0ffee", SkillsListing: listing}}
	if _, err := skillport.Stage(r.root, src, []string{"kwrite"}); err != nil {
		t.Fatal(err)
	}
	return r
}

// A staged skill rides the validate check: its fidelity to the record, its metadata and its links.
func TestStagedSkillsAreValidated(t *testing.T) {
	const staged = "port/cxc/skills/crw-kwrite/"
	const good = "---\nname: cxc-kwrite\ndescription: \"Demo\"\n---\n\nSee [a](references/a.md).\n"
	r := stagedRepo(t, good)
	expectEqual(t, "clean", validate(t, r), result{0, "Validated 2 skills, local link paths and Python syntax.\n", ""})
	r.write(staged+"references/a.md", "# A, changed\n")
	if got := validate(t, r); got.code != 1 || !strings.Contains(got.stderr, staged+"references/a.md: differs from the substituted original") {
		t.Errorf("an unrecorded difference: %+v", got)
	}
	for _, row := range []struct{ name, skill, want string }{
		{"a broken link", good + "See [b](missing.md).\n", staged + "SKILL.md:7: invalid local link missing.md\n"},
		{"bad metadata", "---\nname: cxc-kwrite\ndescription: \"Demo\"\nmetadata: x\n---\n", staged + "SKILL.md: expected one name and one description field\n"},
	} {
		expectEqual(t, row.name, validate(t, stagedRepo(t, row.skill)), result{1, "", row.want})
	}
}
