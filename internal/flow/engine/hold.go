package engine

import (
	"context"
	"fmt"
)

// This file is (g) of the replay rule: a position whose last event is held is never replayed, neither its held CallResult
// nor the crw.hold record after it. When the earlier positions, the input digest, the input snapshot and the effect proof
// (for an executor, the live snapshot of its worktree is P_held) all hold, the call goes on in place: it keeps its call id,
// its cwd and its pair, hold_cleared is written, and what it sends follows the input_state of the held row. When any of them
// fails the held position is the first position of a suffix like any other.

// continueInstruction is the whole input of a turn that continues a call whose packet is already in the thread's history.
const continueInstruction = "The previous turn stopped for %s. Continue the same task. The output schema is unchanged."

// heldResume is a held call a resumed run goes on with in place.
type heldResume struct {
	reason, inputState string
	repairUsed         bool
}

// heldInPlace decides a position whose last event is held: it goes on in place, or it is live like a failed position.
func (c *callRun) heldInPlace(rows []Event, prep Event) bool {
	e := c.e
	held := lastTerminal(rows)
	reason := rowStr(held.Data, "reason")
	// a held worktree failure made nothing of what the call needs: it starts again, in the allocation the journal records
	if reason == ReasonWorktreeFailed || !c.heldStateHolds(rows, prep, held) {
		return c.live()
	}
	info, _ := lookupCall(rows, c.id)
	if reason == "host_infra_activeTurnNotSteerable" && info.ThreadID != "" {
		// the thread has to be idle again: the turn that refused the request is still running otherwise
		if host, ok := e.cfg.Backend.(ReconHost); ok {
			th, err := host.ReadThread(e.ctx, info.ThreadID)
			if err != nil || (th.Status != "idle" && th.Status != "notLoaded") {
				return c.live()
			}
		}
	}
	if !e.claim(c.id) {
		return c.live()
	}
	c.startSeq = prep.Seq
	c.suffixCall = true
	repair, _ := held.Data["repair_used"].(bool)
	c.held = &heldResume{reason: reason, inputState: rowStr(held.Data, "input_state"), repairUsed: repair}
	if m, ok := prep.Data["allocation"].(map[string]any); ok {
		pl := placementFromMap(m)
		c.placement = &pl
		c.placeReq = PlaceRequest{RunID: e.runID, CallID: c.id, Role: c.plan.role, Family: c.plan.spec.Family, Node: c.plan.node, Head: c.plan.opts.Head, Continues: c.plan.resume, Journal: e.j}
	}
	e.j.Append(Event{RunID: e.runID, Kind: KindFlow, CallID: c.id, Path: c.path.id, Position: c.position, Op: "agent", Label: c.label, Event: EvHoldCleared,
		Data: map[string]any{"call_id": c.id, "lineage": e.lineage, "reason": reason, "input_state": c.held.inputState}})
	// the call goes on in this process: it is not superseded, but the positions after it - the crw.hold record among them -
	// are the suffix
	e.markLive(c.path, c.position+1)
	return true
}

// heldStateHolds is (e) for a held call. An executor goes on in place only when its worktree is exactly the state the hold
// recorded: the live snapshot is P_held, which the cumulative proof of the node's calls cannot show (a later call of the script
// on the same worktree, or a commit, can prove an earlier call whose own files are no longer there). A hold whose snapshot could
// not be made (p_held_error) recorded no state to compare with, so the files are not known to be the call's: the position is the
// first of a suffix and the boundary is restored.
func (c *callRun) heldStateHolds(rows []Event, prep Event, held Event) bool {
	e := c.e
	if c.plan.spec.Family != "executor" || c.plan.node == "" {
		return true
	}
	if rowStr(held.Data, "p_held_error") != "" {
		return false
	}
	state := rowStr(held.Data, "p_held")
	if state == "" {
		return true
	}
	if placer, ok := e.cfg.Backend.(Placer); ok {
		cwd := heldCwd(rows, prep)
		if cwd == "" {
			return false
		}
		snap, err := placer.Snapshot(e.ctx, cwd)
		if err != nil || snap != state {
			return false
		}
	}
	return c.effectHolds(held, true)
}

// heldCwd is the directory a held call worked in: its allocation's path, else the directory its thread was started in.
func heldCwd(rows []Event, prep Event) string {
	if m, ok := prep.Data["allocation"].(map[string]any); ok {
		if path := rowStr(m, "path"); path != "" {
			return path
		}
	}
	if info, ok := lookupCall(rows, prep.CallID); ok && info.Cwd != "" {
		return info.Cwd
	}
	return rowStr(prep.Data, "thread_cwd")
}

// resumeHeld goes on with a held call: a call whose packet is in the thread continues with a turn of its own, a call whose
// request was refused sends it for the first time once the host shows that the refused request made nothing.
func (c *callRun) resumeHeld(ctx context.Context) (res CallResult, stop bool, k int, input string, pre *recoveredTurn) {
	e, p, h := c.e, c.plan, c.held
	rows := e.prior.rowsOf(c.id, "agent")
	st := stateOf(c.id, rows)
	info, _ := lookupCall(rows, c.id)
	c.threadID, c.configured, c.cwd = info.ThreadID, info.Configured, info.Cwd
	lastK := 0
	replaced := replacedTurns(rows)
	for _, ev := range rows {
		switch ev.Event {
		case EvPrepared:
			c.turnIDs, lastK, c.delivered = nil, 0, false
		case EvTurnRequested:
			lastK = max(lastK, rowInt(ev.Data, "k"))
		case EvTurnStarted:
			if !replaced[rowStr(ev.Data, "turn_id")] {
				c.turnIDs = append(c.turnIDs, rowStr(ev.Data, "turn_id"))
			}
			c.delivered = true
		}
	}
	c.repairUsed = h.repairUsed
	begin := func(first int) {
		c.firstK = first
		if h.repairUsed {
			c.firstK = 1
		}
	}
	if h.inputState == "delivered" {
		k = lastK + 1
		begin(k)
		return CallResult{}, false, k, fmt.Sprintf(continueInstruction, h.reason), nil
	}
	switch st.last {
	case EvThreadStarted, EvForkStarted:
		begin(1)
		return CallResult{}, false, 1, c.checkpointPreface() + p.prompt, nil
	case EvPrepared:
		if p.resume != nil && !(c.placement != nil && c.placement.Fork) {
			// a continuation on its handle's thread: nothing was created for it
			c.threadID, c.configured = p.resume.ThreadID, p.resume.Configured
			begin(1)
			return CallResult{}, false, 1, p.prompt, nil
		}
	}
	return c.checkHeldRequest(ctx, st, lastK)
}

// checkHeldRequest proves that the request a held call was refused on made nothing (the listing of the call's cwd for a
// creation, the marker of its operation on the thread for a turn) and goes on from what the proof decides. A creation that
// was refused is found by the call's cwd and source; a thread with turns, or a turn carrying the marker, is an outcome nobody
// can prove and holds the node.
func (c *callRun) checkHeldRequest(ctx context.Context, st *callState, lastK int) (res CallResult, stop bool, k int, input string, pre *recoveredTurn) {
	e, p := c.e, c.plan
	synthetic := st.last == EvPrepared
	fork := c.placement != nil && c.placement.Fork
	cwd := p.opts.Cwd
	if c.placement != nil && c.placement.Path != "" {
		cwd = c.placement.Path
	}
	host, ok := e.cfg.Backend.(ReconHost)
	if !ok {
		// a backend that cannot be asked sends it again
		if st.last == EvTurnRequested {
			c.firstK = lastK + 1
			return CallResult{}, false, lastK + 1, rowStr(st.turnReq.Data, "input"), nil
		}
		c.needEstablish = synthetic
		c.send = max(1, rowInt(st.create.Data, "send")) + 1
		if synthetic {
			c.send = 1
		}
		res, stop = c.startOrFork(ctx, fork)
		return res, stop, 1, p.prompt, nil
	}
	st.terminal, st.rejected = "", true
	if synthetic {
		kind := EvThreadRequested
		data := map[string]any{"cwd": cwd, "send": 0}
		if fork {
			kind = EvForkRequested
			data["thread_id"], data["through_turn_id"] = p.resume.ThreadID, p.resume.ThroughTurnID
			data["prefix"] = threadPrefix(e.j, p.resume.ThreadID, p.resume.ThroughTurnID)
		}
		st.create, st.last = Event{Event: kind, Data: data}, kind
		c.needEstablish = true
	}
	r := &reconciler{cfg: ReconcileConfig{RunID: e.runID, Host: host, Now: e.cfg.Now}.withDefaults(), j: e.j, claimed: claimedThreads(e.j)}
	rec := r.apply(ctx, st, r.observe(ctx, st))
	switch rec.Action {
	case RecoverUnknown:
		e.stopNode(p.node, ReasonRecoveryHold)
		return c.unknownResult(), true, 0, "", nil
	case RecoverPending:
		return c.leaveOpen(ReasonDispatchUnknown), true, 0, "", nil
	case RecoverResendCreate:
		if synthetic {
			rec.Send = 1
		}
	case RecoverResendTurn:
		// the refused request made nothing: the packet goes out as the next turn of the call
		rec.K++
	}
	c.rec = rec
	c.firstK = max(rec.K, 1)
	return c.recover(ctx)
}

// startOrFork creates the call's thread the way its placement says.
func (c *callRun) startOrFork(ctx context.Context, fork bool) (CallResult, bool) {
	if fork {
		return c.forkThread(ctx)
	}
	return c.startThread(ctx)
}

// unknownResult is the CallResult of a call the reconcile pass just ended with an unknown outcome.
func (c *callRun) unknownResult() CallResult {
	r, _ := restoreRows(c.e.runID, c.id, CallEvents(c.e.j, c.id), c.e.cfg.OmitSeq)
	return r.res
}
