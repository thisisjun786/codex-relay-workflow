package scenario

import (
	"fmt"
	"strings"
	"testing"
)

// TestADesignWithNoExplorerRecordsTheEndConditionNotRun (CRW-1397 item 5): the end condition commands of the milestone's issues are run
// by the first explorer of the first DESIGN's first audit round (CRW-1382); a DESIGN whose first round spawns no explorer (no open
// question) runs none, and that is recorded: one warning end_condition_not_run on the round's crw.exploreRecord row, naming each command
// and its issue. It is a warning only: the milestone closes. A DESIGN whose explorer was given the commands has no such warning.
func TestADesignWithNoExplorerRecordsTheEndConditionNotRun(t *testing.T) {
	const cmd = "go test ./a -run '^TestA$'"
	warningsOf := func(l *Lab) []string {
		var out []string
		for _, row := range l.Rows("crw.exploreRecord") {
			list, _ := row.Data["warnings"].([]any)
			for _, w := range list {
				out = append(out, fmt.Sprint(w))
			}
		}
		return out
	}
	t.Run("no explorer", func(t *testing.T) {
		nodes := oneNode()
		nodes[0].EndCondition = []string{cmd}
		l := NewLab(t, nodes...)
		got := mustRun(t, l)
		if got.State != "closed" {
			t.Fatalf("state %q: an end condition not run stops nothing", got.State)
		}
		if n := len(l.CallsOf("explorer")); n != 0 {
			t.Fatalf("%d explorers, want none", n)
		}
		var notRun []string
		for _, w := range warningsOf(l) {
			if strings.HasPrefix(w, "end_condition_not_run") {
				notRun = append(notRun, w)
			}
		}
		if len(notRun) != 1 || !strings.Contains(notRun[0], cmd) || !strings.Contains(notRun[0], "CRW-A") {
			t.Fatalf("the end_condition_not_run warnings: %q (all: %q)", notRun, warningsOf(l))
		}
	})
	t.Run("an explorer", func(t *testing.T) {
		nodes := oneNode()
		nodes[0].EndCondition = []string{cmd}
		l := NewLab(t, nodes...)
		d := l.designFor()
		d.OpenQuestions = openQuestions(1)
		l.Design = &d
		l.On("^explore:", func(c *Call) *fakehostPlan {
			answer := map[string]any{"answer": "answer of the explorer", "evidence": []string{"README.md"}, "verified": []string{}, "inferred": []string{}, "unknown": []string{}}
			if strings.Contains(c.Text, "END CONDITIONS") {
				answer["end_conditions"] = []any{map[string]any{"command": cmd, "exit_code": 0, "tests": 1, "summary": "ok"}}
			}
			p := Say(answer)
			return &p
		})
		if got := mustRun(t, l); got.State != "closed" {
			t.Fatalf("state %q", got.State)
		}
		for _, w := range warningsOf(l) {
			if strings.HasPrefix(w, "end_condition_not_run") {
				t.Fatalf("a DESIGN whose explorer was given the commands warns %q", w)
			}
		}
	})
}
