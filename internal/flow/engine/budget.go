package engine

import "encoding/json"

// RowTokens is the token usage a journal row carries in its tokens field, whatever form the journal gave it (a memory journal holds
// the engine's Tokens, a stored row decoded JSON). A row written before a field existed reads it as 0: an old row has no cached
// input and no reasoning output (CRW-1327).
func RowTokens(data map[string]any) Tokens {
	switch t := data["tokens"].(type) {
	case nil:
		return Tokens{}
	case Tokens:
		return t
	case *Tokens:
		if t == nil {
			return Tokens{}
		}
		return *t
	default:
		var out Tokens
		if raw, err := json.Marshal(t); err == nil {
			_ = json.Unmarshal(raw, &out)
		}
		return out
	}
}

// RunSpent is what the run's agent calls spent, by the journal: the sum of the tokens of every terminal row of its agent calls
// (CRW-1278). A call writes one terminal row in each process that ends it and that row carries only what the process spent on it
// (a held call that goes on in place starts again from zero, a replayed call writes no row, and a row another process writes for
// a call - a superseded row of a resume, a reconcile's unknown outcome - carries none), so each token is counted once however
// often the run was resumed. Tokens a process spent on a call it never ended (it was killed mid-call) are in no row and not
// counted, unless a later process goes on with the call from a send_input step whose row records them (call_tokens, CRW-1367): that
// process counts them when it takes them and its terminal row carries them, once.
func RunSpent(events []Event, runID string) Tokens {
	var sum Tokens
	for _, ev := range events {
		if ev.RunID == runID {
			sum = sum.Add(TerminalTokens(ev))
		}
	}
	return sum
}

// TerminalTokens is what one row adds to the tokens a run spent (RunSpent): the tokens of a terminal row of an agent call, nothing for
// any other row. A reading of the calls of a run (`crw flow show`, `ps`, the digest) sums the same rows.
func TerminalTokens(ev Event) Tokens {
	if ev.Kind != KindCall || ev.Op != "agent" || !isTerminal(ev.Event) {
		return Tokens{}
	}
	return RowTokens(ev.Data)
}

// StartedBudget is the token ceiling the run's first process started with, as its started row records it (token_budget), and 0 for a
// run started without one (CRW-1327). The journal fixes it: a resume under another ceiling - a narrower --token-budget, none, the
// ceiling a resume counts from what the run spent - changes the script's budget.total and not this, so what a script decides on
// budget.run (the design loop's token cap) is the same in every process and replays.
func StartedBudget(events []Event, runID string) int64 {
	for _, ev := range events {
		if ev.Kind == KindRun && ev.RunID == runID && ev.Event == "started" {
			return rowInt64(ev.Data, "token_budget")
		}
	}
	return 0
}

// budgetWarned reports whether the journal holds a budget_warning row of the run. A run warns of its budget once (CRW-1278): a resume
// that starts at or past 80% of its ceiling after an earlier process warned writes no second row, and one whose earlier processes
// never warned (they had no ceiling, or spent less) warns when it reaches 80% of its own.
func budgetWarned(events []Event, runID string) bool {
	for _, ev := range events {
		if ev.Kind == KindFlow && ev.RunID == runID && ev.Event == "budget_warning" {
			return true
		}
	}
	return false
}
