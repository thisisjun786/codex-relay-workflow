package dagsched

import (
	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
)

// The limit that decided a pass (dag_passes.deciding_limit): the one that cut the highest-ranked candidate it deferred.
const (
	LimitNone               = "none"
	LimitNoCapacity         = "no_capacity"
	LimitEditOverlap        = "edit_overlap"
	LimitCapacityUnmeasured = "capacity_unmeasured"
)

// Rank is why a ready node stands where it does: the hop count of the longest chain it starts, the live nodes below it, and the stored time
// it became ready (contract 7.5, criterion c2).
type Rank struct {
	CriticalPath, Descendants int
	ReadySince                string
}

// NodeReading is what a reading says of one live node: its derived state (contract 3.1), one disposition and, for every node that is not ready,
// one reason from the closed set (contract 7.1).
type NodeReading struct {
	NodeID, IssueKey, Kind string
	State                  string
	Disposition            string // ready | wait | defer | blocked | skip | done | stale
	Reason                 string // empty for a ready node
	Detail                 string
	Rank                   *Rank  // candidates only
	Stale                  *Stale // a stale node only: what its accepted result no longer matches (invalidation.go)
	// Lifecycle is what the plan says of the node: paused, cancelled or archived. Empty for an active node, and then no key is printed (CRW-281).
	Lifecycle string
	// Release is how an implementation candidate is released and on what basis (CRW-409); nil for a node that was not judged.
	Release *ReleaseJudgement
	// MergeOrder is the node's place in the merge order when a measured conflict of two live nodes asks for one (CRW-410); nil otherwise, and then no key is printed.
	MergeOrder *MergeOrder
}

// PassSummary is the capacity side of one reading.
type PassSummary struct {
	FreeSlots, Ceiling, Held, ReadyCount int
	CeilingSource                        string
	DecidingLimit                        string
	// Overlaps are the overlaps the pass judged, by grade (CRW-409): every implementation candidate that reached the overlap judgement counts the holders it overlaps, by the worst grade of each overlap,
	// whether or not it was released in the end. Counted leaves the mechanical ones out.
	Overlaps OverlapCounts
	// OrderConstraints is the pairs of live nodes the reading puts in a merge order (CRW-410): the later one of each refreshes its base after the earlier one lands.
	OrderConstraints int
}

// Reading is the ready set of one plan at one revision, computed from stored rows only. Ready is in release order; Nodes holds every live
// node, sorted by node id. Two readings of one store state are equal, byte for byte (no clock is read).
type Reading struct {
	PlanID       string
	PlanRevision int64
	StateDigest  string
	InputDigest  string
	Pass         PassSummary
	Ready        []NodeReading
	Nodes        []NodeReading
	// PlanState is paused while the plan is paused; empty for an active plan, and then no key is printed (CRW-281).
	PlanState string
	// ReleasePolicy is the state of local-optimistic release under the plan's release policy (CRW-411); nil when the plan has none, and then no key is printed.
	ReleasePolicy *OptimismState
}

func (n NodeReading) object() contract.OrderedObject {
	o := contract.OrderedObject{
		{Key: "node_id", Value: n.NodeID}, {Key: "issue_key", Value: n.IssueKey}, {Key: "kind", Value: n.Kind},
		{Key: "state", Value: n.State}, {Key: "disposition", Value: n.Disposition},
		{Key: "reason", Value: optionalText(n.Reason)}, {Key: "detail", Value: optionalText(n.Detail)},
	}
	if n.Stale != nil {
		o = append(o, contract.Field{Key: "stale", Value: n.Stale.object()})
	}
	if n.Rank != nil {
		o = append(o, contract.Field{Key: "rank", Value: n.Rank.object()})
	}
	if n.Release != nil {
		o = append(o, contract.Field{Key: "release", Value: n.Release.object()})
	}
	if n.MergeOrder != nil {
		o = append(o, contract.Field{Key: "merge_order", Value: n.MergeOrder.object()})
	}
	if n.Lifecycle != "" {
		o = append(o, contract.Field{Key: "lifecycle", Value: n.Lifecycle})
	}
	return o
}

func (r Rank) object() contract.OrderedObject {
	return contract.OrderedObject{{Key: "critical_path", Value: r.CriticalPath}, {Key: "descendants", Value: r.Descendants}, {Key: "ready_since", Value: optionalText(r.ReadySince)}}
}

func optionalText(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// Object is the reading as the relay prints it (dag-ready).
func (r Reading) Object() contract.OrderedObject {
	ready := make([]any, len(r.Ready))
	for i, n := range r.Ready {
		ready[i] = n.object()
	}
	nodes := make([]any, len(r.Nodes))
	for i, n := range r.Nodes {
		nodes[i] = n.object()
	}
	o := contract.OrderedObject{
		{Key: "ok", Value: true}, {Key: "schema", Value: SchemaReading},
		{Key: "plan_id", Value: r.PlanID}, {Key: "plan_revision", Value: r.PlanRevision},
		{Key: "state_digest", Value: r.StateDigest}, {Key: "input_digest", Value: r.InputDigest},
	}
	if r.PlanState != "" {
		o = append(o, contract.Field{Key: "plan_state", Value: r.PlanState})
	}
	tail := contract.OrderedObject{
		{Key: "pass", Value: contract.OrderedObject{
			{Key: "free_slots", Value: r.Pass.FreeSlots}, {Key: "ceiling", Value: r.Pass.Ceiling}, {Key: "ceiling_source", Value: r.Pass.CeilingSource},
			{Key: "held", Value: r.Pass.Held}, {Key: "ready_count", Value: r.Pass.ReadyCount}, {Key: "deciding_limit", Value: r.Pass.DecidingLimit},
			{Key: "overlap_count", Value: r.Pass.Overlaps.Counted()}, {Key: "overlaps", Value: r.Pass.Overlaps.object()}, {Key: "order_constraints", Value: r.Pass.OrderConstraints},
		}},
	}
	if r.ReleasePolicy != nil {
		tail = append(tail, contract.Field{Key: "release_policy", Value: r.ReleasePolicy.object()})
	}
	return append(append(o, tail...), contract.OrderedObject{{Key: "ready", Value: ready}, {Key: "nodes", Value: nodes}}...)
}

// SchemaReading names the document dag-ready prints.
const SchemaReading = "dag-ready/1"

// find is one node's reading.
func (r Reading) find(id string) (NodeReading, bool) {
	for _, n := range r.Nodes {
		if n.NodeID == id {
			return n, true
		}
	}
	return NodeReading{}, false
}
