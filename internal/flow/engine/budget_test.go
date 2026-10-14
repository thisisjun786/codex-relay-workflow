package engine

import (
	"encoding/json"
	"testing"
)

// TestRunSpentCountsEachTerminalRowOnce (CRW-1278): the run's spent tokens are the sum of the terminal rows of its agent calls, in the
// memory journal's form and in the decoded form of a stored row alike; a row of an older version (no cached input) reads as before,
// rows that are not terminal, not an agent call's or another run's add nothing.
func TestRunSpentCountsEachTerminalRowOnce(t *testing.T) {
	var old map[string]any
	if err := json.Unmarshal([]byte(`{"tokens":{"input":60,"output":10,"total":70}}`), &old); err != nil {
		t.Fatal(err)
	}
	rows := []Event{
		{RunID: "r", Kind: KindCall, Op: "agent", CallID: "a", Event: EvCompleted, Data: old},
		{RunID: "r", Kind: KindCall, Op: "agent", CallID: "b", Event: EvHeld, Data: map[string]any{"tokens": Tokens{Input: 3, Output: 2, Total: 5}}},
		// the held call went on in place in the next process, which spent 40 on it
		{RunID: "r", Kind: KindCall, Op: "agent", CallID: "b", Event: EvTurnStarted, Data: map[string]any{"tokens": Tokens{Total: 999}}},
		{RunID: "r", Kind: KindCall, Op: "agent", CallID: "b", Event: EvCompleted, Data: map[string]any{"tokens": Tokens{Input: 30, Output: 10, Total: 40}}},
		{RunID: "r", Kind: KindCall, Op: "agent", CallID: "c", Event: EvSuperseded, Data: map[string]any{"reason": ReasonSupersededByResume}},
		{RunID: "r", Kind: KindCall, Op: "crw.commit", CallID: "d", Event: EvCompleted, Data: map[string]any{"tokens": Tokens{Total: 7}}},
		{RunID: "other", Kind: KindCall, Op: "agent", CallID: "e", Event: EvCompleted, Data: map[string]any{"tokens": Tokens{Total: 11}}},
	}
	got := RunSpent(rows, "r")
	if want := (Tokens{Input: 93, Output: 22, Total: 115}); got != want {
		t.Errorf("RunSpent = %+v, want %+v", got, want)
	}
}
