package engine

import (
	"fmt"

	"github.com/thisisjun786/codex-relay-workflow/internal/flow/script"
)

// OpLegacyReleaseChild is the operation with which the old project lane started a node's `child` session. The `child` session path is retired
// (CRW-1415): no script calls it and no lane installs it, and the rows an old run wrote with it are read only.
const OpLegacyReleaseChild = "crw.releaseChild"

// LegacyIssueLaneWorkflow is the workflow of an old `child` session's run: the issue lane, retired with the `child` session (CRW-1415).
const LegacyIssueLaneWorkflow = "issue-lane"

// legacyChildRole is the role of the spawn decision the old project lane recorded right before it started a node's `child` session
// (crw.spawnDecision CHILD_2): the current lane records no decision of that role.
const legacyChildRole = "child"

// LegacyIssueChild says why a run is not resumed ("" when nothing in its journal stops it). A run is a legacy `child` session run when
//   - its journal holds a crw.releaseChild row: the old project lane started a node's `child` session;
//   - the old lane released a node as a child (a completed crw.release row of kind child) and recorded the child's spawn decision (a
//     crw.spawnDecision row of role child) for it: it started the child right after, and a process that ended between the start and
//     the crw.releaseChild row left none. The current lane releases a node sized for a child the same way and records no such decision
//     (it escalates the node), so its runs are not taken for legacy ones, whether or not their process ended before the escalation;
//   - it was started as the retired issue lane (its started row records the workflow LegacyIssueLaneWorkflow).
//
// The current project lane sends a node sized for a child up as a size split at the position where the old lane decided and started
// the child, so its script, run again over that journal, would not replay the start: it would escalate the node live and leave the
// child the old lane started running, and every later position on the parent's path would run live; an issue lane no longer exists to
// run. The records stay read only (crw flow show, the summary and the run readers read them); the run is not resumed: engine.Run
// refuses its resume before anything is written (LegacyRun, with the workflow it is resumed as), Recover appends no recovery row to it,
// and every command that would write to the run or start a process for it asks this first.
func LegacyIssueChild(events []Event, runID string) string {
	// the first child release of each node, in the journal's order, and the nodes whose child's spawn was decided
	var released []Event
	childRelease, childSpawn := map[string]bool{}, map[string]bool{}
	for _, ev := range events {
		if ev.RunID != runID {
			continue
		}
		switch {
		case ev.Kind == KindRun && ev.Event == "started" && rowStr(ev.Data, "workflow") == LegacyIssueLaneWorkflow:
			return fmt.Sprintf("run %s is a legacy `child` session run: it was started as the %s workflow of the retired `child` session path (CRW-1415), which no longer exists; %s", runID, LegacyIssueLaneWorkflow, legacyReadOnly)
		case ev.Kind != KindCall:
		case ev.Op == OpLegacyReleaseChild:
			return fmt.Sprintf("run %s is a legacy `child` session run: its journal holds a %s row (call %s, node %s) of the retired `child` session path (CRW-1415), which the current project lane does not replay; %s", runID, OpLegacyReleaseChild, ev.CallID, nodeOr(rowStr(ev.Data, "node")), legacyReadOnly)
		case ev.Event != EvCompleted:
		case ev.Op == opRelease && rowStr(ev.Data, "kind") == legacyChildRole:
			if node := rowStr(ev.Data, "node"); !childRelease[node] {
				childRelease[node] = true
				released = append(released, ev)
			}
		case ev.Op == opSpawnDecision && rowStr(ev.Data, "role") == legacyChildRole:
			childSpawn[rowStr(ev.Data, "node")] = true
		}
	}
	for _, ev := range released {
		if node := rowStr(ev.Data, "node"); childSpawn[node] {
			return fmt.Sprintf("run %s is a legacy `child` session run: the old project lane released node %s as a `child` session (crw.release call %s, kind child) and decided the child's spawn, the start of the retired `child` session path (CRW-1415), which the current project lane does not replay; %s", runID, nodeOr(node), ev.CallID, legacyReadOnly)
		}
	}
	return ""
}

// LegacyRun is LegacyIssueChild with the workflow the run is resumed or written to as (its spec's, or its script's meta.name): a run that
// names LegacyIssueLaneWorkflow is a legacy `child` session run even when its journal holds no row yet, as the run of an old issue-lane process
// that ended after its spec was written and before its started row (CRW-1415, PR #889 round 2).
func LegacyRun(events []Event, runID, workflow string) string {
	if why := LegacyIssueChild(events, runID); why != "" {
		return why
	}
	if workflow == LegacyIssueLaneWorkflow {
		return fmt.Sprintf("run %s is a legacy `child` session run: its spec or its script names the %s workflow of the retired `child` session path (CRW-1415), which no longer exists; %s", runID, LegacyIssueLaneWorkflow, legacyReadOnly)
	}
	return ""
}

// ProgramWorkflow is the workflow a script names: its meta.name, else the program's name.
func ProgramWorkflow(prog *script.Program) string {
	if name, _ := prog.Meta["name"].(string); name != "" {
		return name
	}
	return prog.Name
}

// opSpawnDecision is the op of a spawn decision's row (internal/flow/ops).
const opSpawnDecision = "crw.spawnDecision"

// legacyReadOnly is what the refusal of a legacy `child` session run says of it.
const legacyReadOnly = "the run is not resumed and nothing is written to it; its records stay read only (crw flow show and the summary read them)"

func nodeOr(node string) string {
	if node == "" {
		return "?"
	}
	return node
}
