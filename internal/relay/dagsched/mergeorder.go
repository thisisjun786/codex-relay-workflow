package dagsched

import (
	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
)

// The merge-order constraint (CRW-410). Two live nodes whose heads were measured as conflicting, in a way no rule declared by both settles, cannot both be merged as they are: the one that lands second
// refreshes its base after the one that lands first, and resolves the conflict there. The reading says which is which, and on what measurement. It is a reading and not a gate: nothing here pauses or
// stops a child, changes a disposition or a reason, or refuses a release; the parent reads it when it orders the merges and sends the later candidate back for a base refresh.

// MaxOrderFiles bounds the files a constraint row names.
const MaxOrderFiles = 8

// The lanes a live node is in, in merge order: an open merge turn, an accepted result nothing holds back, and everything else that holds regions.
const (
	LaneTurn     = "turn"
	LaneAccepted = "accepted"
	LaneWorking  = "working"
)

// How sure the reading is that the measurement was made at the heads the nodes have now.
const (
	HeadsCurrentYes     = "yes"
	HeadsCurrentNo      = "no"
	HeadsCurrentUnknown = "unknown"
)

// OrderRow is one measured conflict a constraint rests on, seen from one node: the other node, the observation, how many files do not merge, the files (at most MaxOrderFiles), the worst grade of the
// conflict (local, or exclusive when the declarations say so), the nodes whose declarations do not cover a conflicting file, whether the observation named no files, whether the nodes still have the heads
// it was made at, and the lane and since of the other node.
type OrderRow struct {
	NodeID, ObservationID, Grade, HeadsCurrent, Lane, Since string
	Conflicts                                               int
	Files, DriftNodes                                       []string
	Unattributed                                            bool
}

// TipRow is the latest measurement of a node's head against the tip it lands on, when it conflicts: the base refresh the node needs whatever lands first.
type TipRow struct {
	ObservationID, TipSHA, HeadSource string
	Conflicts                         int
	Files, DriftNodes                 []string
}

// MergeOrder is what the reading says of a node's place in the merge order: the nodes that land before it (After) and after it (Before), each with its observation, the node's own conflict with the tip, and
// the sentence that says what the parent does.
type MergeOrder struct {
	After, Before []OrderRow
	Tip           *TipRow
	Reason        string
}

func stringList(items []string) []any {
	out := make([]any, len(items))
	for i, s := range items {
		out[i] = s
	}
	return out
}

func (r OrderRow) object() contract.OrderedObject {
	return contract.OrderedObject{{Key: "node_id", Value: r.NodeID}, {Key: "observation_id", Value: r.ObservationID}, {Key: "conflicts", Value: r.Conflicts}, {Key: "files", Value: stringList(r.Files)},
		{Key: "grade", Value: r.Grade}, {Key: "drift_nodes", Value: stringList(r.DriftNodes)}, {Key: "unattributed", Value: r.Unattributed}, {Key: "heads_current", Value: r.HeadsCurrent},
		{Key: "lane", Value: r.Lane}, {Key: "since", Value: optionalText(r.Since)}}
}

func (r OrderRow) canonical() map[string]any {
	m := map[string]any{"node_id": r.NodeID, "observation_id": r.ObservationID, "conflicts": r.Conflicts, "files": stringList(r.Files), "grade": r.Grade, "drift_nodes": stringList(r.DriftNodes),
		"unattributed": r.Unattributed, "heads_current": r.HeadsCurrent, "lane": r.Lane}
	if r.Since != "" {
		m["since"] = r.Since
	}
	return m
}

func (t TipRow) object() contract.OrderedObject {
	return contract.OrderedObject{{Key: "observation_id", Value: t.ObservationID}, {Key: "tip_sha", Value: t.TipSHA}, {Key: "head_source", Value: t.HeadSource}, {Key: "conflicts", Value: t.Conflicts},
		{Key: "files", Value: stringList(t.Files)}, {Key: "drift_nodes", Value: stringList(t.DriftNodes)}}
}

func (t TipRow) canonical() map[string]any {
	return map[string]any{"observation_id": t.ObservationID, "tip_sha": t.TipSHA, "head_source": t.HeadSource, "conflicts": t.Conflicts, "files": stringList(t.Files), "drift_nodes": stringList(t.DriftNodes)}
}

func (m MergeOrder) object() contract.OrderedObject {
	after, before := make([]any, len(m.After)), make([]any, len(m.Before))
	for i, r := range m.After {
		after[i] = r.object()
	}
	for i, r := range m.Before {
		before[i] = r.object()
	}
	var tip any
	if m.Tip != nil {
		tip = m.Tip.object()
	}
	return contract.OrderedObject{{Key: "after", Value: after}, {Key: "before", Value: before}, {Key: "tip", Value: tip}, {Key: "reason", Value: m.Reason}}
}

// canonical is the constraint as a pass record and the reading's digest keep it.
func (m MergeOrder) canonical() map[string]any {
	after, before := make([]any, len(m.After)), make([]any, len(m.Before))
	for i, r := range m.After {
		after[i] = r.canonical()
	}
	for i, r := range m.Before {
		before[i] = r.canonical()
	}
	out := map[string]any{"after": after, "before": before, "reason": m.Reason}
	if m.Tip != nil {
		out["tip"] = m.Tip.canonical()
	}
	return out
}

// orderHolder is a node the reading collected as holding its edit regions, with what the lane needs of it.
type orderHolder struct {
	NodeID string
	State  string
	Disp   string
	Acc    Acceptance
	HasAcc bool
	// Held is whether the plan holds the node (it, or the whole plan, is paused, cancelled or archived): its result cannot be merged, so it is not in the merge lane whatever it is accepted as.
	Held bool
}
