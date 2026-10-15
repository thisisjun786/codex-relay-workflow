//go:build dev

package ci

import (
	"fmt"
	"regexp"
	"strings"
	"testing"
)

// CRW-1428 (bundle CRW-1419, N3-2): the relay role ids `supervisor`, `parent` and `child` are names the code keeps
// (a stored row, a host's role policy, a flag, a CLI word). Text a person reads names the layers 부모 and 자식 and
// their English nouns, not the role ids, so the phrase "relay role" (the way the old prose introduced an id) stays
// out of the prose. hierarchy.md keeps the ids in one place, its level-2 section "Names used only in code".
//
// This unit scopes the check to hierarchy.md: the whole file minus that section. The other skill documents are
// widened to by their own units.

// relayRolePhrase is the phrase arm: "relay role" or "relay roles", with any whitespace (a line wrap) between the
// two words. It is matched on the text as written, so a wrapped "relay\nrole" is caught and the line is exact.
var relayRolePhrase = regexp.MustCompile(`(?i)relay\s+roles?\b`)

// codeOnlySection is the heading of the section of hierarchy.md that may hold the phrase: the code-name table.
// headingSection compares its prefix with the whole level-2 heading line, so the "## " belongs to the argument.
const codeOnlySection = "## Names used only in code"

// roleIdProseProblems reports each "relay role" phrase in text, as "rel:line: phrase", outside the sections that
// allow names (each a level-2 heading line as headingSection takes it). An allowed section that is missing or empty
// is a problem too: a section that cannot be found would leave its text in scope or hide a deleted section. The cut
// keeps the line count, so a line number reported is a line of the file.
func roleIdProseProblems(rel, text string, allow []string) []string {
	var problems []string
	for _, heading := range allow {
		section := headingSection(text, heading)
		if strings.TrimSpace(section) == "" {
			problems = append(problems, fmt.Sprintf("%s: section %q is missing or empty", rel, heading))
			continue
		}
		text = strings.Replace(text, section, strings.Repeat("\n", strings.Count(section, "\n")), 1)
	}
	for _, loc := range relayRolePhrase.FindAllStringIndex(text, -1) {
		line := 1 + strings.Count(text[:loc[0]], "\n")
		problems = append(problems, fmt.Sprintf("%s:%d: %q is prose, the role ids live in %q", rel, line, normalized(text[loc[0]:loc[1]]), codeOnlySection))
	}
	return problems
}

func TestRoleIdsStayOutOfProse(t *testing.T) {
	t.Run("hierarchy", func(t *testing.T) {
		text := readRepoText(t, hierarchyFile)
		if problems := roleIdProseProblems(hierarchyFile, text, []string{codeOnlySection}); len(problems) > 0 {
			t.Errorf("hierarchy.md names relay roles outside its code-only section:\n%s", strings.Join(problems, "\n"))
		}
	})

	t.Run("fixtures", func(t *testing.T) {
		allow := []string{codeOnlySection}
		doc := func(prose, code string) string {
			return "# T\n\n## The levels\n\n" + prose + "\n\n" + codeOnlySection + "\n\n" + code + "\n\n## Tail\n\nend\n"
		}
		const phrase = "The relay role id `parent` names the child."
		for name, c := range map[string]struct {
			text string
			want []string // substrings of the problems; none means the text passes
		}{
			"phrase inside the section passes":  {doc("No ids here.", phrase), nil},
			"phrase outside the section fails":  {doc(phrase, "A table."), []string{"x.md:5:"}},
			"wrapped phrase is caught":          {doc("the relay\nrole id `child`", "A table."), []string{"x.md:5:"}},
			"plural and case are caught":        {doc("Relay Roles are listed.", "A table."), []string{"x.md:5:"}},
			"a phrase after the section fails":  {doc("No ids here.", "A table.") + "\nAnother relay role.\n", []string{"x.md:15:"}},
			"no section fails as missing":       {"# T\n\n## The levels\n\nNo ids here.\n", []string{`section "## Names used only in code" is missing or empty`}},
			"empty section fails as empty":      {doc("No ids here.", ""), []string{"is missing or empty"}},
			"an h3 inside the section is in it": {doc("No ids here.", "### Table\n\n"+phrase), nil},
		} {
			t.Run(name, func(t *testing.T) {
				problems := roleIdProseProblems("x.md", c.text, allow)
				if len(c.want) == 0 {
					if len(problems) > 0 {
						t.Fatalf("want a pass, got %v", problems)
					}
					return
				}
				joined := strings.Join(problems, "\n")
				for _, want := range c.want {
					if !strings.Contains(joined, want) {
						t.Errorf("no problem contains %q: %v", want, problems)
					}
				}
			})
		}

		t.Run("the heading without its marks finds no section", func(t *testing.T) {
			// headingSection takes the whole heading line: "Names used only in code" alone is the bug the check
			// above exists to catch, and it must read as a missing section rather than leave the text in scope.
			problems := roleIdProseProblems("x.md", doc("No ids here.", phrase), []string{"Names used only in code"})
			if len(problems) == 0 || !strings.Contains(problems[0], "is missing or empty") {
				t.Fatalf("want the missing-section problem first, got %v", problems)
			}
		})
	})
}
