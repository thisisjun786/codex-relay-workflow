package dag

import "fmt"

// Lifecycle changes (CRW-281): a revision can pause, resume, cancel or archive a node, and pause or resume the plan. They are typed changes
// of the same log as every other change and nothing else records them: there is no row to update and no table of their own. The state they
// leave is the fold of those changes, kept as history rows beside the node and edge rows and read back from the log (loadLifecycle).
//
// A node's lifecycle is not part of the node. The slice digest (contract 4.2) covers a node's spec and its incoming edges and a pause is neither,
// so a pause moves no slice digest and invalidates no manifest and no acceptance (contract 7.4). The state digest covers the lifecycle only when
// it is not the default, so every plan that has none digests as it always did.

// The lifecycle states. A node or plan that is neither paused, cancelled nor archived is active, and active is the empty string.
const (
	LifePaused    = "paused"
	LifeCancelled = "cancelled"
	LifeArchived  = "archived"
)

// LifeRow is one stretch of a lifecycle state in the fold: a node's, or the plan's (NodeID empty), from the revision that introduced it to the
// one that moved it on (RetiredRev, 0 while it is current). An active subject has no row.
type LifeRow struct {
	NodeID        string
	State         string
	IntroducedRev int64
	RetiredRev    int64
}

// transitions are the moves each lifecycle change may make: op -> from state -> to state. A cancelled or archived node has no move out, so a
// cancel is never reverted by the plan: the work is done again with a new node (replace_node).
var transitions = map[string]map[string]string{
	OpPauseNode:   {"": LifePaused},
	OpResumeNode:  {LifePaused: ""},
	OpCancelNode:  {"": LifeCancelled, LifePaused: LifeCancelled},
	OpArchiveNode: {"": LifeArchived, LifePaused: LifeArchived},
	OpPausePlan:   {"": LifePaused},
	OpResumePlan:  {LifePaused: ""},
}

// IsLifecycleOp is whether op is one of the six lifecycle changes.
func IsLifecycleOp(op string) bool { _, ok := transitions[op]; return ok }

// isNodeLifecycleOp is a lifecycle change that names a node; the plan's two carry nothing but their op.
func isNodeLifecycleOp(op string) bool {
	return IsLifecycleOp(op) && op != OpPausePlan && op != OpResumePlan
}

// currentLife is the lifecycle state a subject is in after the rows: "" for active.
func currentLife(rows []LifeRow, node string) string {
	for _, r := range rows {
		if r.NodeID == node && r.RetiredRev == 0 {
			return r.State
		}
	}
	return ""
}

// transitionRefusal says why op cannot move a subject out of from, or "" when it can.
func transitionRefusal(op, subject, from string) string {
	if _, ok := transitions[op][from]; ok {
		return ""
	}
	switch {
	case from == LifeCancelled || from == LifeArchived:
		return fmt.Sprintf("%s is %s, which is final: no change of the plan reopens it, so a cancel is never reverted (to do the work again, replace the node)", subject, from)
	case op == OpResumeNode || op == OpResumePlan:
		return fmt.Sprintf("%s is not paused, so there is nothing to resume", subject)
	}
	return fmt.Sprintf("%s is already paused", subject)
}

// applyLifecycle is the history rows after the lifecycle changes of one revision, in order. A change that is not a legal transition from the state its
// subject is in is an error: the writer judges every change before it gets here (so the fold never sees one), and a log that holds one was not written by
// the writer, which the reader refuses as the corrupt plan it is.
func applyLifecycle(rows []LifeRow, rev int64, changes []Change) ([]LifeRow, error) {
	out := append([]LifeRow(nil), rows...)
	for _, c := range changes {
		if !IsLifecycleOp(c.Op) {
			continue
		}
		node := ""
		if isNodeLifecycleOp(c.Op) {
			node = c.NodeID
		}
		from := currentLife(out, node)
		to, ok := transitions[c.Op][from]
		if !ok {
			subject := "plan"
			if node != "" {
				subject = "node " + node
			}
			return nil, fmt.Errorf("%s: %s", c.Op, transitionRefusal(c.Op, subject, from))
		}
		for i := range out {
			if out[i].NodeID == node && out[i].RetiredRev == 0 {
				out[i].RetiredRev = rev
			}
		}
		if to != "" {
			out = append(out, LifeRow{NodeID: node, State: to, IntroducedRev: rev})
		}
	}
	return out, nil
}

// lifeAt is the lifecycle of the plan ("" when active) and of each node that is not active, as of revision rev.
func lifeAt(rows []LifeRow, rev int64) (plan string, nodes map[string]string) {
	nodes = map[string]string{}
	for _, r := range rows {
		if r.IntroducedRev > rev || (r.RetiredRev != 0 && r.RetiredRev <= rev) {
			continue
		}
		if r.NodeID == "" {
			plan = r.State
		} else {
			nodes[r.NodeID] = r.State
		}
	}
	return plan, nodes
}

// sameLife compares two histories row by row, whatever order they were built in.
func sameLife(a, b []LifeRow) bool {
	if len(a) != len(b) {
		return false
	}
	seen := map[LifeRow]int{}
	for _, r := range a {
		seen[r]++
	}
	for _, r := range b {
		seen[r]--
	}
	for _, n := range seen {
		if n != 0 {
			return false
		}
	}
	return true
}
