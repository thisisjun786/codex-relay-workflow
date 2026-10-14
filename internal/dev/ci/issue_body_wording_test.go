//go:build dev

package ci

import (
	"regexp"
	"strings"
	"testing"
)

// CRW-1326 (the 10-13 trial, CRW-1315 items 4, 18, 35 and 44): an issue body named a build model that the role
// policy contradicted, and a done condition that searched the whole tree matched paths the issue's edit regions
// did not list. The two skills that write issue bodies, crw-add-issue and crw-plan, say that a body names no
// model (the role policy alone does), and run a tree-wide done condition once when the issue is written,
// warning and recording when the paths it matches differ from the edit regions; the write goes on (a
// reversible step is warned and recorded, Jun 10-14). The pair families of the child pair line are policy
// names and stay where the routing table uses them.

var issueBodySkills = []string{"plugins/crw/skills/crw-add-issue/SKILL.md", "plugins/crw/skills/crw-plan/SKILL.md"}

// modelName is a model or model family name that an issue body does not carry.
var modelName = regexp.MustCompile(`(?i)\b(sonnet|haiku|opus|fable|gpt-?\d|gemini|claude|o3|o4-mini|codex-mini)\b`)

func issueBodyRuleProblems(text string) []string {
	flatText := normalized(text)
	var problems []string
	for _, want := range []*regexp.Regexp{
		regexp.MustCompile("An issue body names no model and no effort: the role policy \\(`crw role[^)]*\\) alone decides them"),
		regexp.MustCompile("(?i)done condition[^.]*(searches|checks) the whole (tree|repository)[^.]*run it once when the issue is written"),
		regexp.MustCompile("(?i)paths it matches[^.]*`## 편집 영역`[^.]*warning[^.]*record"),
		regexp.MustCompile("(?i)the issue is still written"),
	} {
		if !want.MatchString(flatText) {
			problems = append(problems, "lacks "+want.String())
		}
	}
	if m := modelName.FindString(text); m != "" {
		problems = append(problems, "names the model "+m)
	}
	return problems
}

func TestIssueBodiesNameNoModel(t *testing.T) {
	for _, skill := range issueBodySkills {
		for _, p := range issueBodyRuleProblems(readRepoText(t, skill)) {
			t.Errorf("%s %s", skill, p)
		}
	}
	t.Run("fixture: a model in the body is caught", func(t *testing.T) {
		text := "An issue body names no model and no effort: the role policy (`crw role helper`, CRW-1316) alone decides them. " +
			"When the done condition searches the whole tree, run it once when the issue is written. " +
			"Compare the paths it matches with `## 편집 영역`; a difference is a warning, and you record it. The issue is still written."
		if p := issueBodyRuleProblems(text); len(p) != 0 {
			t.Errorf("good: %v", p)
		}
		if p := issueBodyRuleProblems(text + " Sonnet builds it."); len(p) != 1 {
			t.Errorf("model: %v", p)
		}
	})
	if strings.Contains(readRepoText(t, "plugins/crw/skills/crw-plan/references/integrations.md"), "## Child pair by issue type") == false {
		t.Error("crw-plan's child pair table moved: the pair families it names are the policy's, read there")
	}
}
