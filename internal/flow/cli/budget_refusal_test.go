package cli

import (
	"strings"
	"testing"
)

// CRW-1392: a run whose budget is spent is not resumed (the tokens over a budget are a cost that cannot be taken back), and the
// refusal says what gives more: a run of a night batch needs a new decision, and a run whose budget the operator gave with
// crw flow start --token-budget, for which no command raises a run's budget, needs a new run with a budget of its own.
func TestRunBudgetRefusalNamesAWay(t *testing.T) {
	night := Spec{RunID: "run-night", Workflow: "lane", TokenBudget: 100, Decision: "ud-night"}
	_, err := runBudget(night, 120, 0)
	if err == nil || !strings.Contains(err.Error(), "budget_exhausted") || !strings.Contains(err.Error(), "needs a new decision") || !strings.Contains(err.Error(), "ud-night") {
		t.Errorf("the night batch's refusal: %v", err)
	}
	operator := Spec{RunID: "run-op", Workflow: "lane", TokenBudget: 100}
	_, err = runBudget(operator, 100, 0)
	if err == nil {
		t.Fatal("a run whose budget is spent was resumed")
	}
	msg := err.Error()
	if strings.Contains(msg, "decision") || !strings.Contains(msg, "budget_exhausted") || !strings.Contains(msg, "crw flow start lane --token-budget") ||
		!strings.Contains(msg, "no command raises") {
		t.Errorf("the refusal of an operator's budget names no way to more budget: %s", msg)
	}
}
