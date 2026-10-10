package spawn

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/role"
)

// CRW-1114 (pre-merge evaluation of 2be6b6f2): a relative skills root contains its skills, and a review word behind Unicode space or
// punctuation is a word start.

// --- CRW-1114 D3 and D4.

func TestSkillsUnderARelativeRootAreContained(t *testing.T) {
	dir := t.TempDir()
	spawnHookMust(t, os.MkdirAll(filepath.Join(dir, "skills", "crw-dev"), 0o755))
	spawnHookMust(t, os.WriteFile(filepath.Join(dir, "skills", "crw-dev", "SKILL.md"), []byte("---\nname: crw-dev\ndescription: Develop.\n---\nBody\n"), 0o644))
	t.Chdir(dir)
	abs := NormalizeSkillMentions("use $crw-dev", filepath.Join(dir, "skills"))
	if got := NormalizeSkillMentions("use $crw-dev", "skills"); got != abs || !strings.Contains(got, "skill://") {
		t.Fatalf("relative root: %q, absolute root: %q", got, abs)
	}
	if got := InlineSkillBodies("use $crw-dev", "skills"); !strings.Contains(got, "Body") {
		t.Fatalf("InlineSkillBodies over a relative root = %q", got)
	}
	if len(SkillBlocks([]string{"use $crw-dev"}, "skills")) == 0 {
		t.Fatal("SkillBlocks over a relative root is empty")
	}
	if got := BuildLeafSkillCatalog("skills"); got != "" && !strings.Contains(got, "crw-dev") {
		t.Fatalf("catalog = %q", got)
	}
}

func TestReviewKeywordBehindUnicodeSpaceOrPunctuationRoutesToTheReviewer(t *testing.T) {
	for message, want := range map[string]role.RoleName{
		"please review the diff": role.Reviewer, // no-break space
		"—review the diff":       role.Reviewer, // em dash
		"see　review":             role.Reviewer, // ideographic space
		"“review” this":          role.Reviewer, // curly quote
		"xéreview":               role.Explorer, // a letter before it: one word
		"café preview":           role.Explorer,
	} {
		if got := InferRole(nil, message); got != want {
			t.Errorf("%q: %s, want %s", message, got, want)
		}
	}
}
