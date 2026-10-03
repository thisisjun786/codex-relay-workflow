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
	r, _ := stagedRepoSource(t, skill)
	return r
}

func stagedRepoSource(t *testing.T, skill string) (*fixtureRepo, skillport.Source) {
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
	return r, src
}

// A staged skill rides the validate check: its fidelity to the record, its metadata and its links.
func TestStagedSkillsAreValidated(t *testing.T) {
	const staged = "port/cxc/skills/crw-kwrite/"
	const good = "---\nname: cxc-kwrite\ndescription: \"Demo\"\n---\n\nSee [a](references/a.md).\n"
	r := stagedRepo(t, good)
	if got := validate(t, r); got.code != 0 || !strings.HasPrefix(got.stdout, "Validated 2 skills, ") || got.stderr != "" { // the rest of the line is the Python rule's, which dev changed
		t.Errorf("clean: %+v", got)
	}
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

func TestRecordedStagedEditsAreValidated(t *testing.T) {
	const staged = "port/cxc/skills/crw-kwrite/"
	const original = "---\nname: cxc-kwrite\ndescription: \"Demo\"\nmetadata: x\n---\n\nSee [a](references/a.md).\n"
	r, src := stagedRepoSource(t, original)
	good := "---\nname: crw-kwrite\ndescription: \"Demo\"\n---\n\nSee [a](references/a.md).\n"
	r.write(staged+"SKILL.md", good)
	if n, err := skillport.RecordEdits(r.root, src, "crw-kwrite", "remove metadata"); err != nil || n != 1 {
		t.Fatalf("RecordEdits = %d, %v", n, err)
	}
	if got := validate(t, r); got.code != 0 || !strings.HasPrefix(got.stdout, "Validated 2 skills, ") {
		t.Fatalf("recorded edit: %+v", got)
	}
	for _, row := range []struct{ skill, want string }{
		{good + "See [b](missing.md).\n", "invalid local link missing.md"},
		{strings.Replace(good, "---\n\n", "metadata: x\n---\n\n", 1), "expected one name and one description field"},
	} {
		r.write(staged+"SKILL.md", row.skill)
		if _, err := skillport.RecordEdits(r.root, src, "crw-kwrite", "test"); err != nil {
			t.Fatal(err)
		}
		if got := validate(t, r); got.code != 1 || !strings.Contains(got.stderr, row.want) {
			t.Errorf("recorded bad skill: %+v", got)
		}
	}
}
