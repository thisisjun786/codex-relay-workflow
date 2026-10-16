package ops

import (
	"encoding/json"
	"slices"

	"github.com/thisisjun786/codex-relay-workflow/internal/flow/engine"
)

// rows is a journal read for the questions the lane operations ask of it.
type rows []engine.Event

// opRows returns the completed rows of an op, in journal order.
func (r rows) opRows(op string) rows {
	var out rows
	for _, ev := range r {
		if ev.Kind == engine.KindCall && ev.Op == op && ev.Event == engine.EvCompleted {
			out = append(out, ev)
		}
	}
	return out
}

// flow returns the flow rows of an event name, in journal order.
func (r rows) flow(event string) rows {
	var out rows
	for _, ev := range r {
		if ev.Kind == engine.KindFlow && ev.Event == event {
			out = append(out, ev)
		}
	}
	return out
}

// where keeps the rows the function accepts.
func (r rows) where(keep func(engine.Event) bool) rows {
	var out rows
	for _, ev := range r {
		if keep(ev) {
			out = append(out, ev)
		}
	}
	return out
}

// last is the newest row the function accepts.
func (r rows) last(keep func(engine.Event) bool) (engine.Event, bool) {
	for i := len(r) - 1; i >= 0; i-- {
		if keep(r[i]) {
			return r[i], true
		}
	}
	return engine.Event{}, false
}

// rangeCalls are the executor calls the commits of node in the range base..head were made from, oldest first, each once: the chain of
// the node's crw.commit rows from the one of head back by their parent, up to base or to a commit no row of the node made.
func (r rows) rangeCalls(node, base, head string) []string {
	commits := r.opRows(OpCommit).where(func(ev engine.Event) bool { return dataStr(ev, "node") == node })
	var calls []string
	seen := map[string]bool{}
	for at := head; at != "" && at != base && !seen[at]; {
		seen[at] = true
		row, ok := commits.last(func(ev engine.Event) bool { return dataStr(ev, "commit") == at })
		if !ok {
			break
		}
		if c := dataStr(row, "call"); c != "" && !slices.Contains(calls, c) {
			calls = append(calls, c)
		}
		at = dataStr(row, "parent")
	}
	slices.Reverse(calls)
	return calls
}

// prepared returns the prepared row of every agent call, in journal order.
func (r rows) prepared() rows {
	return r.where(func(ev engine.Event) bool {
		return ev.Kind == engine.KindCall && ev.Op == "agent" && ev.Event == engine.EvPrepared
	})
}

// terminal is the last terminal row of an agent call (the row its CallResult comes from).
func (r rows) terminal(callID string) (engine.Event, bool) {
	return r.last(func(ev engine.Event) bool {
		if ev.Kind != engine.KindCall || ev.Op != "agent" || ev.CallID != callID {
			return false
		}
		switch ev.Event {
		case engine.EvCompleted, engine.EvNullModel, engine.EvHeld, engine.EvRefused, engine.EvOutcomeUnknown, engine.EvSuperseded:
			return true
		}
		return false
	})
}

// worktreeOf is the path of the node's current attempt worktree: the newest fresh or restore allocation of one of its calls,
// or the newest restored worktree of a resume, whichever the journal wrote last.
func (r rows) worktreeOf(node string) string {
	path, seq := "", 0
	for _, ev := range r {
		switch {
		case ev.Kind == engine.KindCall && ev.Event == engine.EvPrepared && str(ev.Data, "node") == node:
			if m, ok := ev.Data["allocation"].(map[string]any); ok && str(m, "path") != "" && (str(m, "kind") == engine.PlaceFresh || str(m, "kind") == engine.PlaceRestore) && ev.Seq > seq {
				path, seq = str(m, "path"), ev.Seq
			}
		case ev.Kind == engine.KindFlow && ev.Event == engine.EvWorktreeRestored && str(ev.Data, "node") == node && ev.Seq > seq:
			path, seq = str(ev.Data, "path"), ev.Seq
		case ev.Kind == engine.KindCall && ev.Op == OpRelease && ev.Event == engine.EvCompleted && str(ev.Data, "node") == node && str(ev.Data, "kind") == "land" &&
			flag(ev.Data, "released") && str(ev.Data, "worktree") != "" && ev.Seq > seq:
			// the worktree a land release checked the head made outside a run out in (CRW-1354)
			path, seq = str(ev.Data, "worktree"), ev.Seq
		}
	}
	return path
}

// attemptHead is the commit the worktree of the node's current attempt is at for an executor call that has not committed: the node's
// last commit recorded since the attempt's worktree was placed (fresh or restored), else the commit the placement started it at; ""
// when the journal records neither. An executor that commits its own work moves HEAD off it.
func (r rows) attemptHead(node string) string {
	head, seq := "", 0
	for _, ev := range r {
		switch {
		case ev.Kind == engine.KindCall && ev.Event == engine.EvPrepared && str(ev.Data, "node") == node:
			if m, ok := ev.Data["allocation"].(map[string]any); ok && str(m, "path") != "" && (str(m, "kind") == engine.PlaceFresh || str(m, "kind") == engine.PlaceRestore) && ev.Seq > seq {
				head, seq = str(m, "start_commit"), ev.Seq
			}
		case ev.Kind == engine.KindFlow && ev.Event == engine.EvWorktreeRestored && str(ev.Data, "node") == node && ev.Seq > seq:
			head, seq = str(ev.Data, "C"), ev.Seq
		}
	}
	if seq == 0 {
		return ""
	}
	for _, ev := range r {
		if ev.Kind == engine.KindCall && ev.Op == OpCommit && ev.Event == engine.EvCompleted && str(ev.Data, "node") == node && ev.Seq > seq && str(ev.Data, "commit") != "" {
			head = str(ev.Data, "commit")
		}
	}
	return head
}

// resultOf is the JSON the op of a completed row answered.
func resultOf(row engine.Event) json.RawMessage {
	s, _ := row.Data["result"].(string)
	return json.RawMessage(s)
}

// dataStr reads a string of a row's data.
func dataStr(row engine.Event, key string) string { return str(row.Data, key) }

// dataBool reads a flag of a row's data.
func dataBool(row engine.Event, key string) bool { return flag(row.Data, key) }
