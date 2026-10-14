package affordance

import (
	"strings"
	"testing"
)

// TestSessionStartQuestionsRouteAmbiguityUp pins CRW-1340: the SessionStart card no longer routes a
// session to the retired Interview skill or to a synchronous user question; ambiguity during a run
// goes up as a decision request to the session above the project session.
func TestSessionStartQuestionsRouteAmbiguityUp(t *testing.T) {
	ws := t.TempDir()
	card := contextOf(t, RunMapAffordanceSessionStart(payload(ws, "SessionStart", "SESSION", nil), ws, testEnv), "SessionStart")
	questions := RenderQuestionAffordance()
	if !strings.Contains(card, questions) {
		t.Fatalf("the SessionStart card lacks the questions text: %q", card)
	}
	for _, retired := range []string{"crw-interview", "Interview", "`request_user_input`"} {
		if strings.Contains(card, retired) {
			t.Errorf("the SessionStart card names %q: %q", retired, card)
		}
	}
	for _, want := range []string{"decision request", "the session above the project session", "`crw flow report-up`", "do not wait"} {
		if !strings.Contains(questions, want) {
			t.Errorf("the questions text lacks %q: %q", want, questions)
		}
	}
}
