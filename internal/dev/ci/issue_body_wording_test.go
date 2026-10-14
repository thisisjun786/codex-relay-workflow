//go:build dev

package ci

import (
	"path/filepath"
	"regexp"
	"strconv"
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

// issueBodyReferenceGlobs are the references of the two skills that write issue bodies (CRW-1386 d4): the
// child pair line they define is written into an issue body too, so they name no model either. The two pair
// names `Sonnet` and `SOL` are the names the role policy declares for its child pairs (the pair-choice
// command and its quota snapshot key them), so a reference may use those names as names, and never defines a
// pair by the model it runs.
var issueBodyReferenceGlobs = []string{"plugins/crw/skills/crw-add-issue/references/*.md", "plugins/crw/skills/crw-plan/references/*.md"}

// referenceModelName is modelName without the policy's pair name `Sonnet`.
var referenceModelName = regexp.MustCompile(`(?i)\b(haiku|opus|fable|gpt-?\d|gemini|claude|o3|o4-mini|codex-mini)\b`)

// pairByModel is a sentence that defines a pair by the model it runs.
var pairByModel = regexp.MustCompile(`(?i)pair (whose|with a|that runs a) model`)

// codeSpans are the inline code and fenced blocks of a markdown text: a schema key or a JSON example, not
// prose that names a model.
var codeSpans = regexp.MustCompile("(?s)```.*?```|`[^`\n]*`")

func issueBodyReferenceProblems(text string) []string {
	var problems []string
	for i, line := range strings.Split(codeSpans.ReplaceAllString(text, "``"), "\n") {
		if m := referenceModelName.FindString(line); m != "" {
			problems = append(problems, strconv.Itoa(i+1)+": names the model "+m)
		}
		if pairByModel.MatchString(line) {
			problems = append(problems, strconv.Itoa(i+1)+": defines a pair by its model")
		}
	}
	return problems
}

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
	var references []string
	for _, glob := range issueBodyReferenceGlobs {
		matches, err := filepath.Glob(filepath.Join(repoRoot(), glob))
		if err != nil {
			t.Fatal(err)
		}
		references = append(references, matches...)
	}
	if len(references) == 0 {
		t.Fatal("no reference of crw-add-issue or crw-plan was read")
	}
	for _, path := range references {
		rel, _ := filepath.Rel(repoRoot(), path)
		for _, p := range issueBodyReferenceProblems(readRepoText(t, filepath.ToSlash(rel))) {
			t.Errorf("%s:%s", filepath.ToSlash(rel), p)
		}
	}
	t.Run("fixture: a reference names a pair, never a model", func(t *testing.T) {
		good := "The pairs are the names the role policy declares, `Sonnet` and `SOL`: the Sonnet pair, the SOL pair.\n" +
			"    Child pair: Sonnet (reference exists) - a rename\n`claude` is a snapshot key.\n```json\n{\"claude\":1}\n```\n"
		if p := issueBodyReferenceProblems(good); len(p) != 0 {
			t.Errorf("good: %v", p)
		}
		bad := "the Sonnet pair is the declared pair whose model is a Claude Sonnet model\n"
		if p := issueBodyReferenceProblems(bad); len(p) != 2 {
			t.Errorf("bad: %v", p)
		}
	})
	if strings.Contains(readRepoText(t, "plugins/crw/skills/crw-plan/references/integrations.md"), "## Child pair by issue type") == false {
		t.Error("crw-plan's child pair table moved: the pair families it names are the policy's, read there")
	}
}
