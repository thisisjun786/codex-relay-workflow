//go:build dev

package ci

import (
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// CRW-1418 (REFAC1015 B1-4): the issue-child path is retired. A node the architect sized `child` goes up as a `size_split`
// escalation with detail `child_retired`; the issue-lane workflow, `crw flow apply` and `crw flow start --child-host` are gone.
// The six flow documents that described that path no longer name it, and neither do the two skill pages that sent a node to it: crw-plan,
// whose size fields told planners and architects to give an oversized bundle `size_fit` `child`, and crw-run's task packet, which said a
// node a run releases runs the issue lane (PR #889 round 1). Old records (a crw.releaseChild row, an issue-lane run) stay readable, and
// the pages may say so without the words below. The rest of the crw-run skill, other skills, the known-defects records, the refactor
// backlog and the inventory map are not scanned here: they keep their own history or belong to another issue.

// issueChildPathDocs is the documents that may not name the issue-child path.
var issueChildPathDocs = []string{
	"docs/flow/script.md",
	"docs/flow/reporting.md",
	"docs/flow/journal.md",
	canonRefDir + "flow.md",
	canonRefDir + "hierarchy.md",
	canonRefDir + "spawn-policy.md",
	planSkillFile,
	"plugins/crw/skills/crw-run/references/task-packet.md",
}

// issueChildPathWord is a word of the retired issue-child path ("flow apply" as a word of its own: "the workflow apply to" is not it).
var issueChildPathWord = regexp.MustCompile(`SP-CHILD-[12]|issue-lane|\bflow apply|--child-host|child_host|child-host`)

// issueChildPathProblems is each line of text that names the retired issue-child path, with its line number.
func issueChildPathProblems(text string) []string {
	var problems []string
	for i, line := range strings.Split(text, "\n") {
		for _, word := range issueChildPathWord.FindAllString(line, -1) {
			problems = append(problems, "line "+strconv.Itoa(i+1)+" names "+word)
		}
	}
	return problems
}

func TestFlowDocsHaveNoIssueChildPath(t *testing.T) {
	for _, file := range issueChildPathDocs {
		if problems := issueChildPathProblems(readRepoText(t, file)); len(problems) > 0 {
			t.Errorf("%s still names the retired issue-child path:\n  %s", file, strings.Join(problems, "\n  "))
		}
	}

	t.Run("fixture: a clean text passes and each word alone is reported", func(t *testing.T) {
		clean := "A node sized `child` goes up as a `size_split` escalation with detail `child_retired`.\n" +
			"An old `crw.releaseChild` row is read as recorded; the `child` role id names an old record kind.\n" +
			"The rules of the workflow apply to both formats.\n"
		if p := issueChildPathProblems(clean); len(p) > 0 {
			t.Errorf("a clean text is reported: %v", p)
		}
		for _, word := range []string{"SP-CHILD-1", "SP-CHILD-2", "issue-lane", "flow apply", "--child-host", "child_host", "child-host"} {
			p := issueChildPathProblems("clean line\nsee " + word + " here\n")
			if len(p) != 1 || !strings.Contains(p[0], word) || !strings.HasPrefix(p[0], "line 2 ") {
				t.Errorf("%q alone: got %v, want one problem on line 2", word, p)
			}
		}
	})
}
