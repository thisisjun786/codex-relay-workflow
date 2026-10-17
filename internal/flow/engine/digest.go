package engine

import (
	"strings"
)

// The input digest of a call (c of the replay rule) covers everything the call was given: the canonical prompt bytes, the
// schema, the role and its pair, the node, step decision, head and directory options, the QA mark, the context boundary, and the digests
// of the joins settled before it on its path. A position whose digest differs is another call, whatever its prompt says.

// contextBoundary is the conversation a call starts from, the third part of its input digest: {fresh, packet digest} for a
// call that starts a thread, {the handle's thread, its through_turn_id, the digest chain of the journaled results of every
// turn up to that turn} for a continuation. A continuation whose conversation would hold turns outside the replayed prefix
// can therefore never match the digest it recorded. A continuation that had to start from a checkpoint is recorded by the
// checkpoint_started row, not here: the digest is that of the context it asked for.
func (c *callRun) contextBoundary() map[string]any {
	p := c.plan
	if p.resume == nil {
		return map[string]any{"kind": "fresh", "packet": digestJSON([]byte(p.prompt))}
	}
	return map[string]any{
		"kind": "continuation", "thread": p.resume.ThreadID, "through_turn_id": p.resume.ThroughTurnID,
		"chain": threadChain(c.e.j, p.resume.ThreadID, p.resume.ThroughTurnID),
	}
}

// inputDigest is the call's input digest and its context boundary.
func (c *callRun) inputDigest() (string, map[string]any) {
	p := c.plan
	ctxb := c.contextBoundary()
	in := map[string]any{
		"prompt": digestJSON([]byte(p.prompt)), "schema": c.schemaDigest(), "role": p.role, "model": p.spec.Pair.Model, "effort": p.spec.Pair.Effort,
		"node": p.node, "step": p.step, "head": p.opts.Head, "cwd": p.opts.Cwd, "context": ctxb, "joins": c.path.joins,
	}
	// The QA mark changes the overlay, the placement, the snapshot and the native-spawn allowance of the call, so a call that
	// differs from an earlier one only by it is another call (CRW-1206). It is part of the digest only when set, so the digest of
	// every call that is not a QA call is what it was.
	if p.qa {
		in["qa"] = true
	}
	if c.executorAdmission != nil {
		in["executor_task_digest"] = c.executorAdmission.TaskDigest
	}
	return digestJSON(mustJSON(in)), ctxb
}

// threadChain is the digest of a thread's history through a turn, as the journal knows it: the turns in order (id and
// operation) and the result digest of the call each belongs to. The newest attempt of a call is the one that counts, so a
// call that ran again with another result changes the chain of every continuation that follows it.
func threadChain(j Journal, threadID, throughTurn string) string {
	prefix := threadPrefix(j, threadID, throughTurn)
	events := j.Events()
	result := map[string]string{}
	for _, ev := range events {
		if ev.Kind != KindCall || ev.Op != "agent" {
			continue
		}
		switch {
		case ev.Event == EvPrepared:
			delete(result, ev.CallID)
		case ev.Event == EvCompleted:
			result[ev.CallID] = rowStr(ev.Data, "result_digest")
		case isTerminal(ev.Event):
			delete(result, ev.CallID)
		}
	}
	type link struct {
		Turn   string `json:"turn"`
		Op     string `json:"op"`
		Result string `json:"result"`
	}
	chain := make([]link, 0, len(prefix))
	for _, t := range prefix {
		op := rowStr(t, "op")
		callID, _, _ := strings.Cut(op, "/t")
		chain = append(chain, link{Turn: rowStr(t, "turn_id"), Op: op, Result: result[callID]})
	}
	return digestJSON(mustJSON(chain))
}

// readSnapshot reads the plan-owned facts the call sees now (the input snapshot of the replay rule).
func (c *callRun) readSnapshot() string {
	e, p := c.e, c.plan
	if e.cfg.InputSnapshot == nil || p.node == "" {
		return ""
	}
	snap, err := e.cfg.InputSnapshot(e.ctx, SnapshotRequest{RunID: e.runID, CallID: c.id, Node: p.node, Role: p.role, Head: p.opts.Head})
	if err != nil {
		// the facts cannot be read: no replay rests on them
		return "unreadable:" + err.Error()
	}
	return snap
}
