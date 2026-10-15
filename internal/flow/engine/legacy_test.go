package engine

import (
	"strings"
	"testing"
)

// TestLegacyIssueChildPredicate (CRW-1415, PR #889 round 1): a run is a legacy issue-child run when its journal holds a crw.releaseChild
// row; when the old lane released a node as a child (a completed crw.release row of kind child) and decided the child's spawn (a
// crw.spawnDecision row of role child) but the process ended between the child's start and its crw.releaseChild row; and when the run
// was started as the retired issue-lane. A run of the current lane, which releases a node sized for a child the same way and then
// escalates it as a size split (detail child_retired), is not one, whether or not its process ended before the escalation.
func TestLegacyIssueChildPredicate(t *testing.T) {
	const run = "run-1"
	started := func(workflow string) Event {
		return Event{RunID: run, Kind: KindRun, Event: "started", Data: map[string]any{"workflow": workflow}}
	}
	release := func(node, kind string) Event {
		return Event{RunID: run, Kind: KindCall, CallID: "c_rel_" + node, Op: "crw.release", Event: EvCompleted, Data: map[string]any{"node": node, "kind": kind, "released": false, "reason": "size_fit_child"}}
	}
	childSpawn := func(node string) Event {
		return Event{RunID: run, Kind: KindCall, CallID: "c_sp_" + node, Op: "crw.spawnDecision", Event: EvCompleted, Data: map[string]any{"node": node, "role": "child", "decision": "spawn"}}
	}
	escalate := func(node string) Event {
		return Event{RunID: run, Kind: KindFlow, CallID: "c_esc_" + node, Op: "crw.escalate", Event: EvEscalation, Data: map[string]any{"node": node, "reason": "size_split", "detail": "child_retired"}}
	}
	for name, tc := range map[string]struct {
		events []Event
		want   string // a word the refusal names, "" for none
	}{
		"releaseChild row": {[]Event{started("project-lane"), release("A", "child"), childSpawn("A"),
			{RunID: run, Kind: KindCall, CallID: "c_child", Op: OpLegacyReleaseChild, Event: EvCompleted, Data: map[string]any{"node": "A"}}}, OpLegacyReleaseChild},
		"child released and decided, no releaseChild row": {[]Event{started("project-lane"), release("A", "child"), childSpawn("A")}, "node A"},
		"issue-lane run":                            {[]Event{started(LegacyIssueLaneWorkflow)}, LegacyIssueLaneWorkflow},
		"current lane, escalated":                   {[]Event{started("project-lane"), release("A", "child"), escalate("A")}, ""},
		"current lane, ended before the escalation": {[]Event{started("project-lane"), release("A", "child")}, ""},
		"child spawn of another node":               {[]Event{started("project-lane"), release("A", "child"), childSpawn("B")}, ""},
		"another run's rows":                        {[]Event{started("project-lane"), {RunID: "run-2", Kind: KindRun, Event: "started", Data: map[string]any{"workflow": LegacyIssueLaneWorkflow}}}, ""},
	} {
		got := LegacyIssueChild(tc.events, run)
		switch {
		case tc.want == "" && got != "":
			t.Errorf("%s: refused %q", name, got)
		case tc.want != "" && (!strings.Contains(got, "legacy issue-child run") || !strings.Contains(got, tc.want)):
			t.Errorf("%s: %q, want a legacy refusal naming %q", name, got, tc.want)
		}
	}
}
