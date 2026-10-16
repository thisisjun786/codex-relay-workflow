//go:build dev

package ci

import (
	"regexp"
	"strings"
	"testing"
)

// CRW-1511 (bundle CRW-1419, N3-5): the rule for naming work is one file of the shared library,
// crw-canon/references/naming.md. The index table of crw-canon/SKILL.md has a row for it and the three files that
// touch the naming of work (hierarchy.md, tracking-model.md, skill-family.md) link it on a line of their own, so a
// skill that needs the rule follows one link and no other file restates it. The file is read in a public
// repository: it holds no operating path, no host name, no thread id and no relay role phrase (the role ids are
// names the code keeps, see hierarchy.md "Names used only in code").

const namingFile = canonRefDir + "naming.md"

// namingPublicTextProblems are the things a public document does not carry, one entry per kind of hit.
func namingPublicTextProblems(text string) []string {
	var problems []string
	for _, p := range []struct {
		re   *regexp.Regexp
		what string
	}{
		{regexp.MustCompile(`(?i)(^|[\s(\x60"'])/(state|scratch|var/lib|srv|home|Users)/`), "an operating path"},
		{regexp.MustCompile(`(?i)\blina-(os|mac)\b|\bhermes\b`), "a host or operator name"},
		{regexp.MustCompile(`(?i)\b[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}\b`), "a thread id"},
		{regexp.MustCompile(`(?i)relay\s+roles?\b`), "the phrase \"relay role\""},
	} {
		if p.re.MatchString(text) {
			problems = append(problems, "naming.md carries "+p.what)
		}
	}
	return problems
}

func TestNamingRuleIsOneFileAndLinked(t *testing.T) {
	text := readRepoText(t, namingFile)

	// The six rules of the issue, as numbered items of the level-2 section "The rules" (the heading is matched
	// with its "## ", the way headingSection compares a whole heading line).
	rules := headingSection(text, "## The rules")
	if strings.TrimSpace(rules) == "" {
		t.Fatal("naming.md has no section \"## The rules\"")
	}
	numbered := regexp.MustCompile(`(?m)^([0-9]+)\. `).FindAllStringSubmatch(rules, -1)
	if len(numbered) != 6 {
		t.Errorf("naming.md \"The rules\" has %d numbered rules, want 6", len(numbered))
	}
	flat := normalized(rules)
	for _, want := range []string{
		"`OWNER/NAME#N`",
		"`CRW-NNNN`",
		"(tracking-model.md",
		"`CRW-1436(R2)`",
		"`<source identifier>#F<number>`",
		"`codex/crw-NNNN-<short name>`",
		"(hierarchy.md#the-levels)",
		"role id",
		"`cc-q",
	} {
		if !strings.Contains(flat, want) {
			t.Errorf("naming.md \"The rules\" lacks %q", want)
		}
	}
	checks := headingSection(text, "## What the checks do")
	if !strings.Contains(normalized(checks), "warns") || !strings.Contains(normalized(checks), "never refuses") {
		t.Errorf("naming.md \"What the checks do\" does not say the commit-title test only warns and never refuses: %q", checks)
	}
	for _, p := range namingPublicTextProblems(text) {
		t.Error(p)
	}

	// The index table of SKILL.md has a row that links the file.
	skill := readRepoText(t, canonSkillDir+"SKILL.md")
	indexed := false
	for _, table := range markdownTables(skill) {
		for _, row := range table {
			if len(row) > 0 && strings.Contains(row[0], "(references/naming.md)") {
				indexed = true
			}
		}
	}
	if !indexed {
		t.Error("the index table of crw-canon/SKILL.md has no row linking (references/naming.md)")
	}

	// The three sibling files link it on a line of their own.
	for _, rel := range []string{"hierarchy.md", "tracking-model.md", "skill-family.md"} {
		own := false
		for _, line := range lines(readRepoText(t, canonRefDir+rel)) {
			if strings.Contains(line, "](naming.md)") && !strings.HasPrefix(strings.TrimSpace(line), "|") {
				own = true
			}
		}
		if !own {
			t.Errorf("%s does not link (naming.md) on a line of its own", rel)
		}
	}

	t.Run("fixtures", func(t *testing.T) {
		if p := namingPublicTextProblems("A title such as `CRW-1436: text` and `cc-q` tags, see [x](hierarchy.md#the-levels)."); len(p) != 0 {
			t.Errorf("good text: %v", p)
		}
		for bad, want := range map[string]string{
			"kept in /state/crw/mgmt":                     "an operating path",
			"see `/home/jun/code`":                        "an operating path",
			"asked on lina-os":                            "a host or operator name",
			"thread 019a1b2c-3d4e-4f50-8a9b-0c1d2e3f4a5b": "a thread id",
			"the relay\nrole parent":                      "the phrase \"relay role\"",
		} {
			p := namingPublicTextProblems(bad)
			if len(p) != 1 || !strings.Contains(p[0], want) {
				t.Errorf("bad %q: %v, want one problem naming %s", bad, p, want)
			}
		}
	})
}
