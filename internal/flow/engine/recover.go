package engine

import (
	"context"
	"errors"
	"fmt"
)

// RecoverRequest is `crw flow recover CALL --adopt THREAD|TURN | --absent`: the only manual way out of a recovery_hold.
type RecoverRequest struct {
	CallID string
	// Adopt names a thread (the call's creation outcome is unknown) or a turn (its turn outcome is unknown) that the
	// operator says is the call's.
	Adopt string
	// Absent asks reconcile to read the host again with a complete listing and history and to accept the call only if the
	// same proofs hold as for an automatic outcome.
	Absent bool
}

// ProofNotMet is the error of a recovery the host does not support: nothing was appended.
type ProofNotMet struct{ Reason string }

func (e *ProofNotMet) Error() string { return "recover: " + e.Reason }

// RecoverResult says what a recovery did.
type RecoverResult struct {
	CallID string `json:"callId"`
	// Mode is "adopt" or "absent".
	Mode string `json:"mode"`
	// Action is what the call does next (a Recover* action).
	Action   string `json:"action"`
	ThreadID string `json:"threadId,omitempty"`
	TurnID   string `json:"turnId,omitempty"`
	Evidence any    `json:"evidence,omitempty"`
}

// Recover releases the hold of a call whose outcome is unknown. It appends recovery_cleared with the evidence it relied on
// and then the rows the outcome establishes; the next resume goes on from them. It appends nothing when it refuses, and it refuses a legacy
// issue-child run (LegacyIssueChild).
func Recover(ctx context.Context, j Journal, cfg ReconcileConfig, req RecoverRequest) (*RecoverResult, error) {
	if cfg.Host == nil {
		return nil, errors.New("engine: recover needs a host")
	}
	if req.Absent == (req.Adopt != "") {
		return nil, errors.New("recover: exactly one of --adopt THREAD|TURN and --absent is required")
	}
	// a legacy issue-child run is read only (CRW-1415): no recovery row is appended to it
	if why := LegacyIssueChild(j.Events(), cfg.RunID); why != "" {
		return nil, fmt.Errorf("recover: %s", why)
	}
	cfg.Wait = nil // a recovery proves with what the host shows now; it does not sleep through a grace
	cfg = cfg.withDefaults()
	r := &reconciler{cfg: cfg, j: j, claimed: claimedThreads(j)}
	var st *callState
	for _, s := range callStates(j, cfg.RunID) {
		if s.id == req.CallID {
			st = s
		}
	}
	if st == nil {
		return nil, fmt.Errorf("recover: the run has no call %s", req.CallID)
	}
	if !st.held() {
		return nil, &ProofNotMet{Reason: fmt.Sprintf("call %s is not in recovery_hold (its last row is %s)", req.CallID, lastEventName(st))}
	}
	st.terminal = "" // the hold is looked at as the open call it was
	if req.Absent {
		return r.recoverAbsent(ctx, st)
	}
	return r.recoverAdopt(ctx, st, req.Adopt)
}

func lastEventName(st *callState) string {
	if len(st.rows) == 0 {
		return "empty"
	}
	return st.rows[len(st.rows)-1].Event
}

func (r *reconciler) recoverAbsent(ctx context.Context, st *callState) (*RecoverResult, error) {
	p := r.observe(ctx, st)
	switch {
	case p.unknown != "":
		return nil, &ProofNotMet{Reason: "the proof does not hold: " + p.unknown}
	case p.pending:
		return nil, &ProofNotMet{Reason: "the connection that carried the creation request is still alive"}
	}
	r.j.Append(r.callRow(st, EvRecoveryCleared, map[string]any{"mode": "absent", "evidence": p.evidence, "action": p.rec.Action}))
	rec := r.apply(ctx, st, p)
	return &RecoverResult{CallID: st.id, Mode: "absent", Action: rec.Action, ThreadID: rec.ThreadID, TurnID: rec.TurnID, Evidence: p.evidence}, nil
}

func (r *reconciler) recoverAdopt(ctx context.Context, st *callState, target string) (*RecoverResult, error) {
	switch st.last {
	case EvThreadRequested, EvForkRequested:
		return r.adoptThread(ctx, st, target)
	case EvTurnRequested, EvTurnStarted:
		return r.adoptTurn(ctx, st, target)
	}
	return nil, &ProofNotMet{Reason: "call " + st.id + " is not between an intent and its outcome"}
}

// adoptThread takes a thread the operator names for a creation whose outcome is unknown: it has to be at the call's cwd,
// be a root thread of the source the backend connects as (the identity automatic reconcile lists by) and, for a fork, come from the recorded source thread. Its
// history is the operator's to judge; it is recorded.
func (r *reconciler) adoptThread(ctx context.Context, st *callState, id string) (*RecoverResult, error) {
	req := st.create
	fork := req.Event == EvForkRequested
	cwd := rowStr(req.Data, "cwd")
	th, err := r.cfg.Host.ReadThread(ctx, id)
	if err != nil {
		return nil, &ProofNotMet{Reason: fmt.Sprintf("thread %s cannot be read: %v", id, err)}
	}
	switch {
	case th.Cwd != cwd:
		return nil, &ProofNotMet{Reason: fmt.Sprintf("thread %s runs in %s, the call's cwd is %s", id, th.Cwd, cwd)}
	case th.Source != r.cfg.Host.Source():
		return nil, &ProofNotMet{Reason: fmt.Sprintf("thread %s is of the source %q, the backend connects as %q", id, th.Source, r.cfg.Host.Source())}
	case th.ParentThreadID != "":
		return nil, &ProofNotMet{Reason: fmt.Sprintf("thread %s is a sub-agent thread", id)}
	case fork && th.ForkedFromID != rowStr(req.Data, "thread_id"):
		return nil, &ProofNotMet{Reason: fmt.Sprintf("thread %s is not a fork of %s", id, rowStr(req.Data, "thread_id"))}
	case !fork && th.ForkedFromID != "":
		return nil, &ProofNotMet{Reason: fmt.Sprintf("thread %s is a fork", id)}
	}
	turns, err := r.cfg.Host.Turns(ctx, id)
	if err != nil {
		return nil, &ProofNotMet{Reason: fmt.Sprintf("the history of thread %s cannot be read: %v", id, err)}
	}
	ev := EvThreadStarted
	if fork {
		ev = EvForkStarted
	}
	evidence := map[string]any{"operator": true, "history_turns": len(turns)}
	r.j.Append(r.callRow(st, EvRecoveryCleared, map[string]any{"mode": "adopt", "thread_id": id, "evidence": evidence}))
	r.j.Append(r.callRow(st, ev, map[string]any{"thread_id": id, "model": th.Pair.Model, "effort": th.Pair.Effort, "cwd": th.Cwd, "recovered": true, "evidence": evidence}))
	return &RecoverResult{CallID: st.id, Mode: "adopt", Action: RecoverThreadReady, ThreadID: id, Evidence: evidence}, nil
}

// adoptTurn takes a turn the operator names for a call whose turn outcome is unknown. The turn has to be on the call's
// thread and, when its marker can be read, carry the call's operation.
func (r *reconciler) adoptTurn(ctx context.Context, st *callState, id string) (*RecoverResult, error) {
	op := rowStr(st.turnReq.Data, "op")
	turns, err := r.cfg.Host.Turns(ctx, st.thread)
	if err != nil {
		return nil, &ProofNotMet{Reason: fmt.Sprintf("the history of thread %s cannot be read: %v", st.thread, err)}
	}
	var found *HostTurn
	for i := range turns {
		if turns[i].ID == id {
			found = &turns[i]
		}
	}
	switch {
	case found == nil:
		return nil, &ProofNotMet{Reason: fmt.Sprintf("thread %s has no turn %s", st.thread, id)}
	case found.Readable && found.Marker != "" && found.Marker != op:
		return nil, &ProofNotMet{Reason: fmt.Sprintf("turn %s carries the operation %s, not %s", id, found.Marker, op)}
	}
	evidence := map[string]any{"operator": true, "status": found.Status, "marker": found.Marker}
	r.j.Append(r.callRow(st, EvRecoveryCleared, map[string]any{"mode": "adopt", "turn_id": id, "evidence": evidence}))
	// The chosen turn becomes the call's turn: a turn_started row names it. A call that had recorded another turn (the one
	// that became unreadable or missing) replaces it, so the next resume follows the turn the operator chose.
	row := map[string]any{"k": rowInt(st.turnReq.Data, "k"), "op": op, "turn_id": id, "recovered": true, "evidence": evidence}
	switch {
	case st.last == EvTurnRequested:
		r.j.Append(r.callRow(st, EvTurnStarted, row))
	case rowStr(st.turnStarted.Data, "turn_id") != id:
		row["replaces"] = rowStr(st.turnStarted.Data, "turn_id")
		if k := rowInt(st.turnStarted.Data, "k"); k > 0 {
			row["k"] = k
		}
		r.j.Append(r.callRow(st, EvTurnStarted, row))
	}
	return &RecoverResult{CallID: st.id, Mode: "adopt", Action: RecoverTurn, ThreadID: st.thread, TurnID: id, Evidence: evidence}, nil
}
