package ops

import "github.com/thisisjun786/codex-relay-workflow/internal/flow/engine"

// ReleaseRef is a node a run released to an executor: the plan, the node and the execution the release opened.
type ReleaseRef struct {
	Plan, Node, Execution string
}

// OpenReleases are the nodes whose release the run's rows show and whose execution the run did not end: the nodes `crw flow cancel`
// gives up (AgentClose with the flow_cancel reason, which returns the slot) when it ends a run. A node the run gave up (a completed
// crw.escalate row that closed it) or landed (a completed crw.integrate row) after its release is not open. The nodes come in the
// order of their first release.
func OpenReleases(events []engine.Event) []ReleaseRef {
	r := rows(events)
	var out []ReleaseRef
	seen := map[string]int{}
	for _, ev := range r.opRows(OpRelease) {
		if !dataBool(ev, "released") {
			continue
		}
		node := dataStr(ev, "node")
		ref := ReleaseRef{Plan: dataStr(ev, "plan"), Node: node, Execution: dataStr(ev, "execution_id")}
		if i, ok := seen[node]; ok {
			out[i] = ref
			continue
		}
		seen[node] = len(out)
		out = append(out, ref)
	}
	open := out[:0]
	for _, ref := range out {
		release, _ := r.opRows(OpRelease).last(func(ev engine.Event) bool {
			return dataStr(ev, "node") == ref.Node && dataBool(ev, "released")
		})
		if ended, _ := endedByRun(r, release); !ended {
			open = append(open, ref)
		}
	}
	return open
}
