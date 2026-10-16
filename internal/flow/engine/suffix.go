package engine

import (
	"context"
	"fmt"
	"strings"
)

// This file is the boundary of a suffix (engine.resume.suffixRestore). When the first position of a path that fails to
// replay is an executor call (a build, a fix, a continuation of an executor thread), the files it works on are rebuilt as
// they were after the last position that held: the old worktree's HEAD and working snapshot are kept on a superseded ref and
// retired, a new worktree is made at the last commit C with the proven pending tree P of the build or fix after it on top,
// its snapshot is checked against that boundary before any call runs, and a worktree_restored row names it. The node's first
// executor call of the suffix adopts that worktree as its cwd (the allocator never makes a worktree of its own for it, nor
// resets a file); a continuation of a thread of the replayed prefix runs on a fork of it into that worktree, or on a
// checkpoint thread when the thread cannot be forked, and never sends another turn to the original thread whose cwd is the
// retired worktree.

// EvWorktreeRestored is the flow row a suffix restore writes once per node and lineage (see internal/flow/backend/agent).
const EvWorktreeRestored = "worktree_restored"

// Flow rows of a resume.
const (
	// EvHoldCleared is the flow row of a held call a resumed run goes on with in place.
	EvHoldCleared = "hold_cleared"
	// EvWorktreePreserved is the flow row that names the superseded ref an old worktree was kept on.
	EvWorktreePreserved = "worktree_preserved"
	// EvCheckpointStarted is the flow row of a continuation that started a checkpoint thread because its thread could not
	// be forked.
	EvCheckpointStarted = "checkpoint_started"
)

// OpCommit is the op of the rows crw.commit (F08) journals: its completed row names the node and the result commit.
const OpCommit = "crw.commit"

// RestoreRequest asks for the worktree of a node's boundary: a detached worktree at Base with Pending (when set) on top.
type RestoreRequest struct {
	RunID, Node string
	Lineage     int
	// Base is the last commit C of the replayed prefix (the node's creation base when it has none).
	Base string
	// Pending is the proven pending tree P of the build or fix that ends the prefix, "" when it ends with a commit.
	Pending string
	// Journal is the run's journal: the attempts the node has had are read from it.
	Journal Journal
}

// Restored is the worktree a restore made.
type Restored struct {
	Attempt int
	Path    string
	// Digest is the working snapshot checked against the boundary (the tree of Base, or Pending).
	Digest string
}

// PreserveRequest asks for the HEAD and the working snapshot of a worktree to be kept on the superseded ref of a path.
type PreserveRequest struct {
	RunID, Node string
	Lineage     int
	// FlowPath is the path of the first position of the suffix.
	FlowPath string
	// Worktree is the directory to keep.
	Worktree string
	// Held are the states (P_held) the held calls of the suffix recorded: a state that is not the worktree's snapshot now is
	// kept too, on a ref of its own.
	Held []HeldState
}

// HeldState is the P_held a held call recorded.
type HeldState struct{ CallID, Tree string }

// Preserved is the commit that keeps a worktree's state.
type Preserved struct {
	Ref, Commit, Head, Snapshot string
	// HeldRefs are the refs the held states that differ from the snapshot were kept on, by call id.
	HeldRefs map[string]string
}

// Restorer is implemented by a backend that rebuilds the worktree of a node's boundary and keeps the worktree it replaces.
type Restorer interface {
	Restore(ctx context.Context, req RestoreRequest) (Restored, error)
	Preserve(ctx context.Context, req PreserveRequest) (Preserved, error)
}

// boundary is the state of a node's files after the positions that held: the last commit and the pending tree after it.
type boundary struct{ commit, pending string }

// noteExecutor records the pending tree a completed executor call ended on (replayed or live): the files of the node are
// that tree until the next commit.
func (e *Engine) noteExecutor(node, pending string) {
	if node == "" || pending == "" {
		return
	}
	e.boundaryM.Lock()
	defer e.boundaryM.Unlock()
	b := e.boundaries[node]
	if b == nil {
		b = &boundary{}
		e.boundaries[node] = b
	}
	b.pending = pending
}

// noteReplayed records an op row a resumed run replayed.
func (e *Engine) noteReplayed(row Event) {
	e.facts.noteReplayed(row.CallID)
	e.noteOp(row)
}

// noteOp records the commit a completed op row made (replayed or live): the node's files are the commit's tree.
func (e *Engine) noteOp(row Event) {
	if row.Op != OpCommit || row.Event != EvCompleted {
		return
	}
	node, commit := rowStr(row.Data, "node"), rowStr(row.Data, "commit")
	if node == "" || commit == "" {
		return
	}
	e.boundaryM.Lock()
	defer e.boundaryM.Unlock()
	e.boundaries[node] = &boundary{commit: commit}
}

// allocationOf is a worktree the journal records for a node: the allocation of a prepared row or a restored worktree.
type nodeAllocation struct {
	path, start string
	seq         int
}

// nodeAllocations reads the worktrees the journal records for a node, in order.
func nodeAllocations(events []Event, node string) []nodeAllocation {
	var out []nodeAllocation
	for _, ev := range events {
		switch {
		case ev.Kind == KindCall && ev.Event == EvPrepared && rowStr(ev.Data, "node") == node:
			if m, ok := ev.Data["allocation"].(map[string]any); ok {
				if k := rowStr(m, "kind"); (k == PlaceFresh || k == PlaceRestore) && rowStr(m, "path") != "" {
					out = append(out, nodeAllocation{path: rowStr(m, "path"), start: rowStr(m, "start_commit"), seq: ev.Seq})
				}
			}
		case ev.Kind == KindFlow && ev.Event == EvWorktreeRestored && rowStr(ev.Data, "node") == node:
			out = append(out, nodeAllocation{path: rowStr(ev.Data, "path"), start: rowStr(ev.Data, "C"), seq: ev.Seq})
		}
	}
	return out
}

// restoreBoundary rebuilds the boundary state of the call's node before the call's prepared row, when the call is the node's
// first executor call of a suffix and the node has been worked on before. It returns the reason and detail of the hold when
// it cannot (the call then ends held for a worktree failure and nothing is sent).
func (c *callRun) restoreBoundary() (reason, detail string) {
	e, p := c.e, c.plan
	if e.cfg.Resume == nil || !c.suffixCall || p.node == "" || p.refusal != nil || c.held != nil || c.rec != nil {
		return "", ""
	}
	if !c.touchesExecutorWorktree() {
		return "", ""
	}
	rs, ok := e.cfg.Backend.(Restorer)
	if !ok {
		return "", ""
	}
	e.restoreM.Lock()
	defer e.restoreM.Unlock()
	if e.restored[p.node] {
		return "", ""
	}
	allocs := nodeAllocations(e.j.Events(), p.node)
	if len(allocs) == 0 {
		return "", ""
	}
	ctx := e.ctx
	// A turn the previous process left running in the worktree is interrupted and confirmed first: nothing writes to the old
	// worktree when its state is kept.
	if f := c.endOpenCalls(ctx); f != nil {
		e.stopNode(p.node, ReasonRecoveryHold)
		return ReasonRecoveryHold, "a turn of the superseded suffix could not be confirmed interrupted: " + f.detail
	}
	base, pending := e.boundaryOf(p.node, allocs)
	if base == "" {
		return ReasonWorktreeFailed, "the boundary of node " + p.node + " has no base commit"
	}
	last := allocs[len(allocs)-1]
	if c.worktreeExists(last.path) {
		kept, err := rs.Preserve(ctx, PreserveRequest{RunID: e.runID, Node: p.node, Lineage: e.lineage, FlowPath: c.path.id, Worktree: last.path, Held: e.heldStates(p.node)})
		if err != nil {
			return ReasonWorktreeFailed, "the superseded worktree " + last.path + " could not be kept: " + err.Error()
		}
		e.j.Append(Event{RunID: e.runID, Kind: KindFlow, CallID: c.id, Path: c.path.id, Position: c.position, Op: "agent", Label: c.label, Event: EvWorktreePreserved,
			Data: map[string]any{"node": p.node, "lineage": e.lineage, "worktree": last.path, "ref": kept.Ref, "commit": kept.Commit, "head": kept.Head, "snapshot": kept.Snapshot, "held_refs": kept.HeldRefs}})
	}
	made, err := rs.Restore(ctx, RestoreRequest{RunID: e.runID, Node: p.node, Lineage: e.lineage, Base: base, Pending: pending, Journal: e.j})
	if err != nil {
		return ReasonWorktreeFailed, "the boundary of node " + p.node + " could not be restored: " + err.Error()
	}
	e.restored[p.node] = true
	var pendingValue any
	if pending != "" {
		pendingValue = pending
	}
	e.j.Append(Event{RunID: e.runID, Kind: KindFlow, CallID: c.id, Path: c.path.id, Position: c.position, Op: "agent", Label: c.label, Event: EvWorktreeRestored,
		Data: map[string]any{"node": p.node, "lineage": e.lineage, "attempt": made.Attempt, "path": made.Path, "C": base, "P": pendingValue, "snapshot_digest": made.Digest}})
	return "", ""
}

// heldStates are the P_held states the node's held calls recorded and nothing has superseded yet.
func (e *Engine) heldStates(node string) []HeldState {
	var out []HeldState
	for _, id := range e.prior.order {
		rows := e.prior.rowsOf(id, "agent")
		if len(rows) == 0 || rowStr(rows[0].Data, "node") == "" {
			continue
		}
		st := stateOf(id, rows)
		if st.node != node || st.terminal != EvHeld {
			continue
		}
		if tree := rowStr(lastTerminal(rows).Data, "p_held"); tree != "" {
			out = append(out, HeldState{CallID: id, Tree: tree})
		}
	}
	return out
}

// touchesExecutorWorktree is whether the call runs in an executor's worktree: an executor that creates a thread, or a
// continuation of an executor thread.
func (c *callRun) touchesExecutorWorktree() bool {
	if c.plan.spec.Family == "executor" {
		return true
	}
	return c.plan.resume != nil && c.plan.resume.Family == "executor"
}

// worktreeExists is whether the directory of an old worktree is still there.
func (c *callRun) worktreeExists(path string) bool {
	if w, ok := c.e.cfg.Backend.(interface{ WorktreeExists(string) bool }); ok {
		return w.WorktreeExists(path)
	}
	return path != ""
}

// boundaryOf is the commit and the pending tree the node's files have after the positions of this resume that held or ran:
// the last commit (the node's first allocation base when none) and the pending tree of the build or fix after it.
func (e *Engine) boundaryOf(node string, allocs []nodeAllocation) (commit, pending string) {
	e.boundaryM.Lock()
	b := e.boundaries[node]
	e.boundaryM.Unlock()
	if b != nil {
		commit, pending = b.commit, b.pending
	}
	if commit == "" {
		for _, a := range allocs {
			if a.start != "" {
				commit = a.start
				break
			}
		}
	}
	return commit, pending
}

// refPart makes a flow path one safe component of a ref name.
func refPart(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		ch := s[i]
		switch {
		case ch >= 'a' && ch <= 'z', ch >= 'A' && ch <= 'Z', ch >= '0' && ch <= '9', ch == '_', ch == '-', ch == '@', ch == '+', ch == '=', ch == ',':
			b.WriteByte(ch)
		default:
			fmt.Fprintf(&b, "%%%02X", ch)
		}
	}
	return b.String()
}

// RetiredRef names the saved HEAD and working snapshot of an executor attempt.
// Run and node ids use the same ref encoding as superseded snapshots, including
// synthetic milestone ids such as "milestone:M1".
func RetiredRef(runID, node string, attempt int) string {
	return fmt.Sprintf("refs/crw/flow/%s/retired/%s/a%d", refPart(runID), refPart(node), attempt)
}

// SupersededRef is the ref the HEAD and the working snapshot of an old worktree are kept on:
// refs/crw/flow/<run>/superseded/<lineage>/<path>, one commit per lineage and path of the flow (the node's id follows the
// path, so two nodes of one path keep one each). The path is percent-encoded into one component, so the ref of a path is
// never a prefix of the ref of another.
func SupersededRef(runID string, lineage int, flowPath, node string) string {
	leaf := refPart(flowPath)
	if node != "" {
		leaf += "@" + refPart(node)
	}
	return fmt.Sprintf("refs/crw/flow/%s/superseded/%d/%s", refPart(runID), lineage, leaf)
}

// SupersededHeldRef is the ref the P_held of a held call is kept on when it differs from the snapshot of the worktree that
// was kept: beside the worktree's ref, named by the call.
func SupersededHeldRef(runID string, lineage int, flowPath, node, callID string) string {
	return SupersededRef(runID, lineage, flowPath, node) + "-held-" + refPart(callID)
}
