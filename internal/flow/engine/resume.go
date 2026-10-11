package engine

import (
	"context"
	"encoding/json"
	"fmt"
)

// This file is the part of a resumed run that F05 needs to be true: the calls the previous process ended are not run
// again, the calls it left in the middle go on from what reconcile decided, and a node starts its next attempt only after
// the calls the previous process left open on it have ended superseded. The whole replay rule (engine.resume.rule, F07) is
// not here: a call is compared with its journal rows by its input digest, its schema digest and its pair, and nothing is
// proven against the working tree.

// placementFromMap is the inverse of Placement.Map: the allocation a prepared row recorded.
func placementFromMap(m map[string]any) Placement {
	fork, _ := m["fork"].(bool)
	return Placement{Kind: rowStr(m, "kind"), Attempt: rowInt(m, "attempt"), Lineage: rowInt(m, "lineage"), Path: rowStr(m, "path"),
		Start: rowStr(m, "start_commit"), Pending: rowStr(m, "pending"), Digest: rowStr(m, "snapshot_digest"), Fork: fork}
}

func (c *callRun) schemaDigest() string {
	if c.plan.schema == nil {
		return ""
	}
	return digestJSON(c.plan.schema)
}

// resumePrior looks the call up in the journal of the process that ran before. It returns true when the call needs no
// prepared row of its own: its result is replayed (c.replay), or it goes on from what reconcile decided (c.rec).
func (c *callRun) resumePrior() bool {
	e, p := c.e, c.plan
	var rows []Event
	var prep Event
	for _, ev := range CallEvents(e.j, c.id) {
		if ev.Op != "agent" || ev.Seq >= e.startSeq {
			continue
		}
		rows = append(rows, ev)
		if ev.Event == EvPrepared {
			prep = ev
		}
	}
	if len(rows) == 0 || prep.Seq == 0 {
		return false
	}
	d := prep.Data
	same := rowStr(d, "input_digest") == digestJSON([]byte(p.prompt)) && rowStr(d, "schema_digest") == c.schemaDigest() &&
		rowStr(d, "role") == p.role && rowStr(d, "model") == p.spec.Pair.Model && rowStr(d, "effort") == p.spec.Pair.Effort
	if !same {
		return false
	}
	st := stateOf(c.id, rows)
	switch st.terminal {
	case EvCompleted, EvNullModel, EvRefused, EvOutcomeUnknown:
		c.startSeq = prep.Seq
		res := c.replayResult(rows)
		c.replay = &res
		return true
	case "":
		rec := e.recovery[c.id]
		if rec == nil || rec.Action == RecoverUnknown {
			return false
		}
		c.startSeq, c.rec = prep.Seq, rec
		e.claim(c.id)
		if m, ok := d["allocation"].(map[string]any); ok {
			pl := placementFromMap(m)
			c.placement = &pl
			c.placeReq = PlaceRequest{RunID: e.runID, CallID: c.id, Role: p.role, Family: p.spec.Family, Node: p.node, Head: p.opts.Head, Continues: p.resume, Journal: e.j}
		}
		return true
	}
	return false
}

// replayResult is the CallResult of a call the journal ended: what the rows of its last terminal event say.
func (c *callRun) replayResult(rows []Event) CallResult {
	info, _ := LookupCall(c.e.j, c.id)
	c.threadID, c.configured, c.cwd = info.ThreadID, info.Configured, info.Cwd
	var term Event
	replaced := replacedTurns(rows)
	for _, ev := range rows {
		switch {
		case ev.Event == EvPrepared:
			c.turnIDs = nil
		case ev.Event == EvTurnStarted:
			if !replaced[rowStr(ev.Data, "turn_id")] {
				c.turnIDs = append(c.turnIDs, rowStr(ev.Data, "turn_id"))
			}
		case isTerminal(ev.Event):
			term = ev
		}
	}
	c.repairUsed, _ = term.Data["repair_used"].(bool)
	if raw, err := json.Marshal(term.Data["tokens"]); err == nil {
		_ = json.Unmarshal(raw, &c.tokens)
	}
	return c.result(terminalEvents[term.Event], rowStr(term.Data, "reason"), term.Seq, term.Data)
}

// supersedePrior ends, before the call starts a thread of its own, the calls the previous process left open on its node
// (and the call's own earlier rows when its input changed) as superseded. A turn one of them may still be running is
// interrupted first and the interruption confirmed with thread/read: a new attempt never starts beside a live turn of the
// attempt it replaces. A turn that cannot be confirmed interrupted holds the call.
func (c *callRun) supersedePrior(ctx context.Context) (CallResult, bool) {
	e := c.e
	if e.cfg.Resume == nil {
		return CallResult{}, false
	}
	host, _ := e.cfg.Backend.(ReconHost)
	for _, st := range callStatesBefore(e.j, e.runID, e.startSeq) {
		if st.terminal != "" || (st.id != c.id && (c.plan.node == "" || st.node != c.plan.node)) {
			continue
		}
		if st.id != c.id && e.claimedCall(st.id) {
			continue // the resumed script took its position over: it goes on from what reconcile decided
		}
		if host != nil && st.thread != "" && (st.last == EvTurnRequested || st.last == EvTurnStarted) {
			if err := c.interruptAndConfirm(ctx, host, st); err != nil {
				data := map[string]any{"input_state": c.inputState(), "detail": err.Error()}
				e.stopNode(c.plan.node, ReasonRecoveryHold)
				return c.terminal(EvHeld, StatusHeld, ReasonRecoveryHold, data), true
			}
		}
		first := st.rows[0]
		e.j.Append(Event{RunID: e.runID, Kind: KindCall, CallID: st.id, Path: first.Path, Position: first.Position, Op: "agent", Label: first.Label, Event: EvSuperseded,
			Data: map[string]any{"reason": ReasonSupersededByAttempt, "by": c.id, "last_event": st.last}})
	}
	return CallResult{}, false
}

// interruptAndConfirm interrupts the turn the call left running and waits until thread/read shows the thread no longer
// active. A turn that is not running needs nothing.
func (c *callRun) interruptAndConfirm(ctx context.Context, host ReconHost, st *callState) error {
	turns, err := host.Turns(ctx, st.thread)
	if err != nil {
		return fmt.Errorf("the turns of thread %s cannot be read: %w", st.thread, err)
	}
	op := rowStr(st.turnReq.Data, "op")
	turnID := ""
	for _, t := range turns {
		if (st.last == EvTurnStarted && t.ID == rowStr(st.turnStarted.Data, "turn_id")) || (st.last == EvTurnRequested && t.Readable && t.Marker == op) {
			if t.Status == "inProgress" {
				turnID = t.ID
			}
		}
	}
	if turnID == "" {
		return nil
	}
	if err := host.InterruptTurn(ctx, st.thread, turnID); err != nil {
		return fmt.Errorf("turn %s of thread %s could not be interrupted: %w", turnID, st.thread, err)
	}
	o := c.e.cfg.Resume.timings()
	for i := 0; i < 50; i++ {
		th, err := host.ReadThread(ctx, st.thread)
		if err == nil && th.Status != "active" {
			return nil
		}
		if err := o.Wait(ctx, o.Poll); err != nil {
			return err
		}
	}
	return fmt.Errorf("the interruption of turn %s of thread %s could not be confirmed", turnID, st.thread)
}

// timings fills the defaults of a resume's timings.
func (o *ResumeOptions) timings() ResumeOptions {
	out := *o
	if out.Wait == nil {
		out.Wait = SleepWait
	}
	if out.Poll <= 0 {
		out.Poll = defaultReconPoll / 10
	}
	return out
}
