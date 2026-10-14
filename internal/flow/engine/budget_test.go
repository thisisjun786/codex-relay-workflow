package engine

import (
	"encoding/json"
	"fmt"
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

// TestSessionTokensKeepTheCachedSplit (CRW-1327): a session call's tokens are the growth of its subagent's rollout total in every
// part (cached input and reasoning output too), its terminal row records them in the journal's form (cached_input,
// reasoning_output), and a row of an older version without them reads them as 0.
func TestSessionTokensKeepTheCachedSplit(t *testing.T) {
	k := newSessionKit(t)
	k.host.tokens["/rollout/agent-1"] = Tokens{Input: 4171534, CachedInput: 4006518, Output: 46728, ReasoningOutput: 31200, Total: 4218262}
	res := k.h.run(`const a = await agent.call('design', { agentType: 'explorer', label: 'a' })
return [a.meta.tokens.input, a.meta.tokens.cached_input, a.meta.tokens.output, a.meta.tokens.reasoning_output, a.meta.tokens.total]`)
	if got := value[[]int64](t, res); fmt.Sprint(got) != "[4171534 4006518 46728 31200 4218262]" {
		t.Fatalf("the call's tokens %v", got)
	}
	rows := k.rowsOf("a")
	done := rows[len(rows)-1]
	raw, err := json.Marshal(done.Data["tokens"])
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != `{"input":4171534,"cached_input":4006518,"output":46728,"reasoning_output":31200,"total":4218262}` {
		t.Errorf("the %s row records %s", done.Event, raw)
	}
	var old map[string]any
	if err := json.Unmarshal([]byte(`{"tokens":{"input":80,"output":20,"total":100}}`), &old); err != nil {
		t.Fatal(err)
	}
	if got := RowTokens(old); got != (Tokens{Input: 80, Output: 20, Total: 100}) || got.FreshInput() != 80 {
		t.Errorf("an old row reads %+v (fresh %d)", got, got.FreshInput())
	}
}
