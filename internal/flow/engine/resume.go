package engine

import (
	"context"
	"encoding/json"
	"errors"
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

// resumePrior looks the call up in the journal of the process that ran before, at its own (path, position). It returns true
// when the call needs no prepared row of its own: its result is replayed (c.replay), it goes on from what reconcile decided
// (c.rec), or it is a held call that goes on in place (c.held). Every other call runs live and, with it, the rest of its path.
func (c *callRun) resumePrior() bool {
	e, p := c.e, c.plan
	rows := e.prior.rowsOf(c.id, "agent")
	var prep Event
	for _, ev := range rows {
		if ev.Event == EvPrepared {
			prep = ev
		}
	}
	if len(rows) == 0 || prep.Seq == 0 {
		return c.live()
	}
	// A call whose outcome is unknown, and whose hold nobody has cleared, is not run again whatever the walk says: a request of
	// it may still be at work on the host, so a new attempt of the call would send it twice and lose the hold (a new prepared
	// row ends the state the hold is read from). Its unknown outcome stays its result, and the node stays in recovery_hold.
	if stateOf(c.id, rows).held() {
		if c.path.suffix || rowStr(prep.Data, "input_digest") != c.digest {
			c.e.markLive(c.path, c.position+1)
		}
		return c.replayFrom(rows, prep)
	}
	// (c) and (d): the input digest, the context boundary and the joins before the position, and the plan-owned input snapshot.
	// A node that was accepted is not rebuilt silently when its own spec or packet changed: the call is refused with the route.
	changed := rowStr(prep.Data, "input_digest") != c.digest || rowStr(prep.Data, "input_snapshot") != c.snapshot
	if changed && p.refusal == nil && e.nodeAccepted(p.node) {
		p.refusal = &Refusal{Reason: ReasonAcceptedNodeChanged, Detail: acceptedNodeRoute(p.node)}
	}
	// (a) and (b): an earlier position of the path did not replay, or the path was opened by a call after one that did not
	if c.path.suffix || changed {
		return c.live()
	}
	st := stateOf(c.id, rows)
	if p.refusal != nil {
		// a call refused before it opened ends the same way on every run: it replays when the previous process refused it for
		// the same reason
		if st.terminal == EvRefused && rowStr(lastTerminal(rows).Data, "reason") == p.refusal.Reason {
			return c.replayFrom(rows, prep)
		}
		return c.live()
	}
	switch st.terminal {
	case EvCompleted, EvNullModel, EvRefused, EvOutcomeUnknown:
		// (e): the effect of what the call produced is still there
		if !c.effectHolds(lastTerminal(rows), false) {
			return c.live()
		}
		return c.replayFrom(rows, prep)
	case EvHeld:
		return c.heldInPlace(rows, prep)
	case "":
		rec := e.recovery[c.id]
		if rec == nil || rec.Action == RecoverUnknown {
			return c.live()
		}
		if !e.claim(c.id) {
			// a new attempt of the node ended this call superseded before the script reached its position: the position
			// runs as a call of its own
			return c.live()
		}
		c.startSeq, c.rec = prep.Seq, rec
		c.suffixCall = true
		if m, ok := prep.Data["allocation"].(map[string]any); ok {
			pl := placementFromMap(m)
			c.placement = &pl
			c.placeReq = PlaceRequest{RunID: e.runID, CallID: c.id, Role: p.role, Family: p.spec.Family, Node: p.node, Head: p.opts.Head, QA: p.qa, Continues: p.resume, Journal: e.j}
		}
		// the call goes on in this process: it is not superseded, but the positions after it are the suffix
		e.markLive(c.path, c.position+1)
		return true
	}
	return c.live()
}

// live is the end of the walk for the call's path: the call runs live, and so does every later position of the path.
func (c *callRun) live() bool {
	c.suffixCall = true
	c.e.markLive(c.path, c.position)
	return false
}

// replayFrom delivers the result the call's rows restore.
func (c *callRun) replayFrom(rows []Event, prep Event) bool {
	c.startSeq = prep.Seq
	res := c.replayResult(rows)
	c.replay = &res
	c.e.facts.noteReplayed(c.id)
	term := lastTerminal(rows)
	if term.Event == EvCompleted {
		c.e.noteExecutor(c.plan.node, rowStr(term.Data, "pending_tree"))
	}
	return true
}

// lastTerminal is the last terminal row of a call's rows.
func lastTerminal(rows []Event) Event {
	var term Event
	for _, ev := range rows {
		switch {
		case ev.Event == EvPrepared:
			term = Event{}
		case isTerminal(ev.Event):
			term = ev
		case reopensCall(ev.Event):
			term = Event{}
		}
	}
	return term
}

// nodeAccepted is whether the previous processes recorded an acceptance of the node that nothing has superseded since.
func (e *Engine) nodeAccepted(node string) bool {
	if node == "" || e.prior == nil {
		return false
	}
	for _, id := range e.prior.order {
		rows := e.prior.calls[id]
		last := rows[len(rows)-1]
		if last.Op == OpAccept && last.Event == EvCompleted && rowStr(last.Data, "node") == node {
			return true
		}
	}
	return false
}

// OpAccept is the op of the rows crw.accept (F08) journals: its completed row names the node.
const OpAccept = "crw.accept"

// acceptedNodeRoute says what to do when a node that was accepted has another spec or packet now. A stale result is never
// reused silently.
func acceptedNodeRoute(node string) string {
	return fmt.Sprintf("node %s was accepted and its spec or packet changed: record a plan revision with update_node (the acceptance becomes stale) and run again, or correct an oversized child with dag-correct", node)
}

// effectHolds is (e) of the replay rule for an executor call: the pending tree (or P_held) it recorded is still the state of
// the node's files, or a recorded commit carries it. A call that is not an executor's, or recorded no state, has no effect to
// prove. held is whether the row is a held row, whose state is P_held.
func (c *callRun) effectHolds(term Event, held bool) bool {
	e := c.e
	if c.plan.spec.Family != "executor" || c.plan.node == "" {
		return true
	}
	key := "pending_tree"
	if held {
		key = "p_held"
	}
	state := rowStr(term.Data, key)
	if state == "" {
		return true
	}
	if !held && e.committedTree(c.plan.node, state) {
		return true
	}
	prover := e.prover()
	if prover == nil {
		return true
	}
	np, err := e.nodeProof(prover, c.plan.node)
	return err == nil && np != nil && np.Proven(c.id)
}

// committedTree is whether a recorded commit of the node carries the tree: the build or fix that produced it is proven by
// that commit.
func (e *Engine) committedTree(node, tree string) bool {
	for _, id := range e.prior.order {
		for _, ev := range e.prior.calls[id] {
			if ev.Op == OpCommit && ev.Event == EvCompleted && rowStr(ev.Data, "node") == node && rowStr(ev.Data, "tree") == tree {
				return true
			}
		}
	}
	return false
}

func (e *Engine) prover() Prover {
	if e.cfg.Prover != nil {
		return e.cfg.Prover
	}
	p, _ := e.cfg.Backend.(Prover)
	return p
}

// nodeProof reads the proof of a node's executor calls once per resume: the rows it reads are the previous processes'.
func (e *Engine) nodeProof(p Prover, node string) (NodeProof, error) {
	e.proofM.Lock()
	defer e.proofM.Unlock()
	if np, ok := e.proofs[node]; ok {
		return np, nil
	}
	var rows []Event
	for _, ev := range e.j.Events() {
		if ev.Seq < e.startSeq {
			rows = append(rows, ev)
		}
	}
	np, err := p.ProveNode(e.ctx, rows, node)
	if err != nil {
		return nil, err
	}
	e.proofs[node] = np
	return np, nil
}

// replayResult is the CallResult of a call the journal ended: what the rows of its last terminal event say.
func (c *callRun) replayResult(rows []Event) CallResult {
	r, _ := restoreRows(c.e.runID, c.id, rows, c.e.cfg.OmitSeq)
	c.threadID, c.configured, c.cwd, c.turnIDs, c.tokens, c.repairUsed = r.threadID, r.configured, r.cwd, r.turnIDs, r.tokens, r.repairUsed
	return r.res
}

// restored is a call as its journal rows say it ended.
type restored struct {
	res        CallResult
	threadID   string
	configured Pair
	cwd        string
	turnIDs    []string
	tokens     Tokens
	repairUsed bool
}

// RestoreCallResult is the CallResult of a call as its rows say it ended, and nothing else: the output is the canonical
// result the completed row stored, the status and reason are those of the last terminal event (held included, which has no
// output), and the meta is the references the rows carry (role, pairs, thread, turns, tokens). The host is not read. rows
// are the rows of the one call, in journal order; ok is false for rows that hold no prepared and no terminal row.
func RestoreCallResult(runID, callID string, rows []Event, omitSeq bool) (CallResult, bool) {
	r, ok := restoreRows(runID, callID, rows, omitSeq)
	return r.res, ok
}

// reopensCall is a non-terminal row that makes a call open again after a terminal one (see stateOf).
func reopensCall(ev string) bool {
	switch ev {
	case EvThreadRequested, EvForkRequested, EvThreadStarted, EvForkStarted, EvTurnRequested, EvTurnStarted, EvRecoveryCleared:
		return true
	}
	return false
}

func restoreRows(runID, callID string, rows []Event, omitSeq bool) (restored, bool) {
	// The result is that of the call's newest attempt: a prepared row starts an attempt, and a row that opens the call again
	// (a thread or turn intent, a started thread or turn, a recovery_cleared after an unknown outcome) ends the terminal
	// state the rows had reached (the same rule as stateOf). A call that is open now has no result, and the terminal row of
	// an earlier attempt is never combined with the metadata of a later one.
	var prep, term Event
	for _, ev := range rows {
		if ev.Kind != KindCall || ev.CallID != callID {
			continue
		}
		switch {
		case ev.Event == EvPrepared:
			prep, term = ev, Event{}
		case isTerminal(ev.Event):
			term = ev
		case reopensCall(ev.Event):
			term = Event{}
		}
	}
	if prep.Seq == 0 || term.Seq == 0 {
		return restored{}, false
	}
	info, _ := lookupCall(rows, callID)
	r := restored{threadID: info.ThreadID, configured: info.Configured, cwd: info.Cwd}
	replaced := replacedTurns(rows)
	for _, ev := range rows {
		if ev.Kind != KindCall || ev.CallID != callID {
			continue
		}
		switch {
		case ev.Event == EvPrepared:
			r.turnIDs = nil
		case ev.Event == EvTurnStarted:
			if !replaced[rowStr(ev.Data, "turn_id")] {
				r.turnIDs = append(r.turnIDs, rowStr(ev.Data, "turn_id"))
			}
		}
	}
	r.repairUsed, _ = term.Data["repair_used"].(bool)
	if raw, err := json.Marshal(term.Data["tokens"]); err == nil {
		_ = json.Unmarshal(raw, &r.tokens)
	}
	startSeq, endSeq := prep.Seq, term.Seq
	if omitSeq {
		startSeq, endSeq = 0, 0
	}
	status, reason := terminalEvents[term.Event], rowStr(term.Data, "reason")
	r.res = CallResult{
		CallID: callID, Path: prep.Path, Position: prep.Position, Label: prep.Label, Status: status, Reason: reason,
		Meta: CallMeta{
			Role: rowStr(prep.Data, "role"), RequestedPair: Pair{Model: rowStr(prep.Data, "requested_model"), Effort: rowStr(prep.Data, "requested_effort")},
			ConfiguredPair: r.configured, ThreadID: r.threadID, TurnIDs: append([]string{}, r.turnIDs...), RepairUsed: r.repairUsed, Tokens: r.tokens,
			StartSeq: startSeq, EndSeq: endSeq,
		},
	}
	if r.threadID != "" {
		r.res.Handle = Handle(runID, callID)
	}
	if out, ok := term.Data["result"].(string); ok && status == StatusCompleted {
		r.res.Output = json.RawMessage(out)
	}
	return r, true
}

// supersedePrior ends, before the call starts a thread of its own, the calls the previous process left open on its node
// (and the call's own earlier rows when its input changed) as superseded. A turn one of them may still be running is
// interrupted first and the interruption confirmed with thread/read: a new attempt never starts beside a live turn of the
// attempt it replaces. A turn that cannot be confirmed interrupted holds the call.
func (c *callRun) supersedePrior(ctx context.Context) (CallResult, bool) {
	e := c.e
	switch f := c.endOpenCalls(ctx); {
	case f == nil:
		return CallResult{}, false
	case f.cancelled:
		return c.terminal(EvSuperseded, StatusSuperseded, ReasonCancelled, nil), true
	default:
		e.stopNode(c.plan.node, ReasonRecoveryHold)
		return c.terminal(EvHeld, StatusHeld, ReasonRecoveryHold, map[string]any{"input_state": c.inputState(), "detail": f.detail}), true
	}
}

// supersedeFailure is why the open calls of the previous process could not be ended: the run was cancelled, or a turn could
// not be confirmed interrupted.
type supersedeFailure struct {
	cancelled bool
	detail    string
}

// endOpenCalls ends the open calls of the previous process on the call's node (and the call's own) as superseded, the
// interruption of a running turn first. The resume's reason is the call's own earlier attempt; a call of the node's earlier
// attempt is ended by the new attempt.
func (c *callRun) endOpenCalls(ctx context.Context) *supersedeFailure {
	e := c.e
	if e.cfg.Resume == nil {
		return nil
	}
	host, _ := e.cfg.Backend.(ReconHost)
	for _, st := range callStatesBefore(e.j, e.runID, e.startSeq) {
		if st.id != c.id && (c.plan.node == "" || st.node != c.plan.node) {
			continue
		}
		if st.terminal == EvHeld {
			// A held call is not superseded (its P_held is kept with the old worktree), but a turn of it may still run: the
			// host shows that none does, or the new attempt does not start. A call the resumed script took over proved it at
			// its own position and goes on there.
			if host != nil && (st.id == c.id || !e.claimedByScript(st.id)) {
				if err := e.heldTurnsEnded(ctx, host, st.rows); err != nil {
					return &supersedeFailure{detail: err.Error()}
				}
			}
			continue
		}
		if st.terminal != "" {
			continue
		}
		op, owner, skip := e.beginSupersede(st.id, st.id == c.id)
		if skip {
			continue // the resumed script took its position over: it goes on from what reconcile decided
		}
		if !owner {
			// another attempt ends this call: nothing starts on the node until it has
			select {
			case <-op.done:
			case <-ctx.Done():
				return &supersedeFailure{cancelled: true}
			}
			if op.err != nil {
				return &supersedeFailure{detail: op.err.Error()}
			}
			continue
		}
		if host != nil && st.thread != "" && (st.last == EvTurnRequested || st.last == EvTurnStarted) {
			if err := c.interruptAndConfirm(ctx, host, st); err != nil {
				e.endSupersede(st.id, op, err)
				return &supersedeFailure{detail: err.Error()}
			}
		}
		first := st.rows[0]
		reason, data := ReasonSupersededByAttempt, map[string]any{"by": c.id, "last_event": st.last}
		if st.id == c.id {
			reason = ReasonSupersededByResume
			data["lineage"] = e.lineage
		}
		data["reason"] = reason
		e.supersededRow(st.id, st.rows, first, data)
		e.endSupersede(st.id, op, nil)
	}
	return nil
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
	if err := c.e.awaitInactive(ctx, host, st.thread); err != nil {
		return fmt.Errorf("the interruption of turn %s of thread %s could not be confirmed: %w", turnID, st.thread, err)
	}
	return nil
}

// awaitInactive waits until thread/read shows the thread no longer active, polling at the resume's pace.
func (e *Engine) awaitInactive(ctx context.Context, host ReconHost, thread string) error {
	o := e.cfg.Resume.timings()
	for i := 0; i < 50; i++ {
		th, err := host.ReadThread(ctx, thread)
		if err == nil && th.Status != "active" {
			return nil
		}
		if err := o.Wait(ctx, o.Poll); err != nil {
			return err
		}
	}
	return errors.New("thread/read still shows it active")
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
