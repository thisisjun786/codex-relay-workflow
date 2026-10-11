// Package shadow computes what a workflow script would have decided on a recorded run and compares it with what the parent
// decided, and measures a recorded run. It reads a crw-flow-trace/1 file (internal/flow/trace) and calls no agent: the script
// runs on the engine over a replay backend that answers every call from the trace.
package shadow

import (
	"encoding/json"

	"github.com/thisisjun786/codex-relay-workflow/internal/flow/engine"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/trace"
)

// The decision kinds.
const (
	KindSpawn      = "spawn"
	KindTier       = "tier"
	KindFlush      = "flush"
	KindEscalation = "escalation"
)

// Decision is one decision of a run, on either side of the comparison.
type Decision struct {
	Kind string `json:"kind"`
	Node string `json:"node,omitempty"`
	// Role is the role a spawn or a flush starts, or the role a tier step moves the node to.
	Role   string `json:"role,omitempty"`
	Family string `json:"family,omitempty"`
	// Pair is the model and effort the call is configured with, "model/effort".
	Pair   string `json:"pair,omitempty"`
	Reason string `json:"reason,omitempty"`
	CallID string `json:"callId,omitempty"`
	Label  string `json:"label,omitempty"`
	// Seq is the journal row the decision was read from, 0 for a decision read from a recorded decision row.
	Seq int `json:"seq,omitempty"`
	// Order is the 1-based place of the decision in its side's sequence.
	Order int `json:"order"`
}

// Derive reads the decisions of a run from its journal rows, in row order. A prepared row of an agent call that continues a
// handle is not a decision (the thread exists); any other prepared row is a spawn, or a flush when the call's output schema
// is one of batchSchemas (the digests of the batch verdict schema) and its role is a reviewer. A crw.tierStep call is a tier
// decision on the node of the executor-t1 call that carries its step id. An engine escalation row is an escalation. A call that
// a resumed run prepared again is one decision: its first prepared row counts.
func Derive(events []engine.Event, batchSchemas map[string]bool) []Decision {
	stepNode := map[string]string{}
	for _, e := range events {
		if e.Kind == engine.KindCall && e.Event == engine.EvPrepared && e.Op == "agent" {
			if id := strData(e.Data, "step_decision"); id != "" {
				if _, ok := stepNode[id]; !ok {
					stepNode[id] = strData(e.Data, "node")
				}
			}
		}
	}
	var out []Decision
	prepared := map[string]bool{}
	for _, e := range events {
		switch {
		case e.Kind == engine.KindCall && e.Event == engine.EvPrepared && e.Op == "agent":
			if strData(e.Data, "resume_of") != "" || prepared[e.CallID] {
				continue
			}
			prepared[e.CallID] = true
			d := Decision{Kind: KindSpawn, Node: strData(e.Data, "node"), Role: strData(e.Data, "role"), Family: strData(e.Data, "family"),
				Pair: strData(e.Data, "model") + "/" + strData(e.Data, "effort"), CallID: e.CallID, Label: e.Label, Seq: e.Seq}
			if d.Family == "reviewer" && batchSchemas[strData(e.Data, "schema_digest")] {
				d.Kind = KindFlush
			}
			out = append(out, d)
		case e.Kind == engine.KindCall && e.Event == engine.EvCompleted && e.Op == "crw.tierStep":
			id := strData(e.Data, "step_decision")
			out = append(out, Decision{Kind: KindTier, Node: stepNode[id], Role: "executor-t1", Family: "executor", CallID: e.CallID, Label: e.Label, Seq: e.Seq})
		case e.Kind == engine.KindFlow && e.Event == engine.EvEscalation:
			out = append(out, Decision{Kind: KindEscalation, Node: strData(e.Data, "node"), Reason: strData(e.Data, "reason"), CallID: e.CallID, Seq: e.Seq})
		}
	}
	return number(out)
}

// Actual are the decisions the recorded run made: those its journal rows hold, and those the run recorded in rows of their own
// (flow_step_decisions, the batch flushes of flow_spawn_decisions, the escalations of dag_agent_closures) that the journal rows
// do not already account for. A recorded row is counted once: per node, a kind has the larger of the number of journal
// decisions and the number of recorded rows.
func Actual(run *trace.Run, batchSchemas map[string]bool) []Decision {
	events := make([]engine.Event, len(run.Events))
	for i, e := range run.Events {
		events[i] = e.Engine(run.RunID)
	}
	out := Derive(events, batchSchemas)
	have := map[string]int{}
	for _, d := range out {
		have[d.Kind+"\x00"+d.Node]++
	}
	seen := map[string]int{}
	for _, r := range run.Decisions {
		var d Decision
		switch {
		case r.Source == "flow_step_decisions":
			d = Decision{Kind: KindTier, Node: r.Node, Role: r.ToTier, Reason: r.Rule}
		case r.Source == "flow_spawn_decisions" && r.Kind == KindFlush:
			d = Decision{Kind: KindFlush, Node: r.Node, Role: "reviewer", Family: "reviewer", Reason: r.Rule, CallID: r.CallID}
		case r.Source == "dag_agent_closures" && r.Kind == KindEscalation:
			d = Decision{Kind: KindEscalation, Node: r.Node, Reason: r.Reason}
		default:
			continue
		}
		key := d.Kind + "\x00" + d.Node
		seen[key]++
		if seen[key] > have[key] {
			out = append(out, d)
		}
	}
	return number(out)
}

func number(ds []Decision) []Decision {
	for i := range ds {
		ds[i].Order = i + 1
	}
	return ds
}

func strData(m map[string]any, key string) string {
	s, _ := m[key].(string)
	return s
}

// batchSchema reports whether a strict output schema is the batch verdict: an object whose "nodes" array holds one block per
// member, each naming its node and head.
func batchSchema(schema []byte) bool {
	var s struct {
		Properties map[string]struct {
			Items struct {
				Properties map[string]json.RawMessage `json:"properties"`
			} `json:"items"`
		} `json:"properties"`
	}
	if json.Unmarshal(schema, &s) != nil {
		return false
	}
	nodes, ok := s.Properties["nodes"]
	if !ok {
		return false
	}
	_, hasNode := nodes.Items.Properties["node"]
	_, hasHead := nodes.Items.Properties["head"]
	return hasNode && hasHead
}
