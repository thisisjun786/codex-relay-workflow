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
// counted.
func RunSpent(events []Event, runID string) Tokens {
	var sum Tokens
	for _, ev := range events {
		if ev.Kind != KindCall || ev.RunID != runID || ev.Op != "agent" || !isTerminal(ev.Event) {
			continue
		}
		sum = sum.Add(RowTokens(ev.Data))
	}
	return sum
}
