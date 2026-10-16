package skill

import (
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/layername"
)

// TestSkillTextNamesTheLayers (CRW-1478): the base-refresh and parent-title descriptions, the parent-title malformed_role
// requirement, the issue-ready design-first reason and the start-policy selftest lines name the layer (자식 for the relay
// parent, 부모 for the relay supervisor) and carry no bare role word.
func TestSkillTextNamesTheLayers(t *testing.T) {
	check := func(what, text string, want ...string) {
		t.Helper()
		if hits := layername.Find(text); len(hits) != 0 {
			t.Errorf("%s: bare role words %v in %q", what, hits, text)
		}
		for _, w := range want {
			if !strings.Contains(text, w) {
				t.Errorf("%s does not name %q: %q", what, w, text)
			}
		}
	}
	check("the base-refresh description", baseRefresh.description, "A 자식 that updates a pull request branch itself", "exactly two parent commits", "a reversed order of the parent commits")
	check("the parent-title description", parentTitle.description, "Compute a 자식's task title (project session)", "자식's Codex task title leads with")

	got, err := titleDecide(map[string]any{"role": "orchestrator"})
	if err != nil {
		t.Fatal(err)
	}
	requires, _ := got["requires"].([]string)
	if got["reason"] != "malformed_role" || len(requires) != 1 {
		t.Fatalf("an unknown role: %v", got)
	}
	check("the parent-title malformed_role requirement", requires[0], "[\"parent\", \"supervisor\", \"child\"] (자식, 부모, `child` session)")

	report, err := readyReportFor(readyInput{ID: "CRW-SYN", Title: "t", Labels: labelList{"design-first"}})
	if err != nil {
		t.Fatal(err)
	}
	reason := ""
	for _, r := range report.Reasons {
		if strings.HasPrefix(r, "design first: the issue carries the label") {
			reason = r
		}
	}
	check("the issue-ready design-first reason", reason, "the architect's proposal and the 자식's decision")

	var names []string
	for _, c := range selfCases {
		names = append(names, c.name)
	}
	check("the start-policy selftest cases", strings.Join(names, "\n"), "자식 G", "자식 H", "자식 I", "자식 J", "a parked 자식 given a running 자식's path")
}
