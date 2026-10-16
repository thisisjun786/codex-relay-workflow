package engine

import (
	"fmt"
	"path"
	"slices"
	"strings"

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

// The retired node reviewer and reviewer-final paths (CRW-1429). The engine cannot import ops: these are the strings ops records under,
// and an external test pins them to ops.OpReview, ops.OpReviewerFinal, ops.OpArchive and the rule ids of ops/rules.go.
const (
	// OpLegacyReview is the record of the old node reviewer's VERDICT (lib/lane.js review); no built-in script calls it.
	OpLegacyReview = "crw.review"
	// OpLegacyReviewerFinal is the record of the old close's reviewer-final VERDICT_FINAL (lib/close.js finalReview); no built-in script
	// calls it.
	OpLegacyReviewerFinal = "crw.reviewerFinal"
	// OpLegacyArchive is the archive of the reviewer threads the old close made after its audit (settleReviews of crw:lib/close before
	// CRW-1447); the current close audits the acceptances and archives nothing.
	OpLegacyArchive = "crw.archive"
	// opAudit and opEscalate are the ops of the old close's settle step before its archive: the audit, and the audit_mismatch
	// escalation on a mismatch.
	opAudit    = "crw.audit"
	opEscalate = "crw.escalate"
)

// LegacyRetiredRules are the spawn rules of the retired paths: the node reviewer (SP-REV-1), the candidate batch review (SP-REV-2),
// reviewer-final (SP-REVF-1) and the explorer of a reviewer's evidence question (SP-EXPL-3).
var LegacyRetiredRules = []string{"SP-REV-1", "SP-REV-2", "SP-REVF-1", "SP-EXPL-3"}

// LegacyLaneWorkflows are the built-in workflows that ran the lane library (crw:lib/lane, crw:lib/close): the project lane, the landing of
// a head made outside a run, and the night batch, which nests the project lane under its own run id. A workflows test pins the list to
// the built-ins.
var LegacyLaneWorkflows = []string{"project-lane", "land-head", "night-batch"}

// The roles of the retired reviewer calls.
const (
	legacyReviewerRole      = "reviewer"
	legacyReviewerFinalRole = "reviewer-final"
)

// legacyLanePath is a lane a run nested with workflow(): the subtree of its child path and the workflow_opened row that opened it.
type legacyLanePath struct {
	id     string
	label  string
	callID string
}

func (p legacyLanePath) holds(rowPath string) bool {
	return rowPath == p.id || strings.HasPrefix(rowPath, p.id+"/")
}

// nestedWorkflowName is the name of the workflow a workflow_opened label names: `workflow:<name>` for a workflow('name') call, and for a
// {scriptPath} call `workflow:<scriptPath>`, whose name is the base name of the path without `.js` (a script nests a sibling file by its
// name, which resolves to `<name>.js` in its own directory).
func nestedWorkflowName(label string) string {
	return strings.TrimSuffix(path.Base(strings.TrimPrefix(label, "workflow:")), ".js")
}

// LegacyReviewerRun says why a run is a legacy run of the retired node reviewer or reviewer-final path (CRW-1429), "" when it is not.
// The current project lane runs the unit check where the old lane called the node reviewer, the crosscheck where the old close called
// reviewer-final, and its report where the old close archived the reviewer threads, so its script, run again over such a journal, would
// not replay those positions: it would run them live, and every later position of their path after them (the report would be sent
// twice). A run is one when, for its run id,
//   - its journal holds a row of OpLegacyReview or OpLegacyReviewerFinal (no built-in script calls them; only an old one wrote them);
//   - inside the lane scope, its journal holds an agent call of role reviewer or reviewer-final, or a completed crw.spawnDecision row of a
//     retired rule (LegacyRetiredRules): what an old lane wrote before the record of the review, when its process ended in between;
//   - inside the lane scope, its journal holds a row of OpLegacyArchive: the old close archived the reviewer threads after its audit,
//     where the current close goes on to its report;
//   - anywhere in the run, a row of OpLegacyArchive follows a crw.audit row on the same path, at the position right before it or, with a
//     crw.escalate row between, the one before that: the settle step of the old close (its audit, the audit_mismatch escalation on a
//     mismatch, the archive), whatever workflow ran it, so an old lane nested under another name and a script that settled the same way
//     are refused too. An archive of a user workflow that does not follow an audit in that way stays allowed.
//
// The lane scope is the rows an old built-in lane wrote: the whole run when its started row names a workflow of LegacyLaneWorkflows,
// and the subtree of every lane the run nested with workflow() (a workflow_opened row whose label names a workflow of the list, by name
// or by its script path's base name): every row whose path is the child path nestedPathID gives or lies under it. A reviewer call of a
// user workflow outside any nested lane, or under a nested workflow that is not a lane, is no legacy row: the role key stays usable.
// The reason names the row (its op or role, its call id, its node or milestone, and for a nested lane its label and its workflow_opened
// call) and the retired path. The records stay read only, as a legacy `child` session run's do.
func LegacyReviewerRun(events []Event, runID string) string {
	whole := false
	var lanes []legacyLanePath
	for _, ev := range events {
		if ev.RunID != runID {
			continue
		}
		switch {
		case ev.Kind == KindRun && ev.Event == "started":
			whole = whole || slices.Contains(LegacyLaneWorkflows, rowStr(ev.Data, "workflow"))
		case ev.Kind == KindFlow && ev.Op == "workflow" && ev.Event == "workflow_opened" && slices.Contains(LegacyLaneWorkflows, nestedWorkflowName(ev.Label)):
			lanes = append(lanes, legacyLanePath{id: nestedPathID(ev.Path, ev.Position), label: ev.Label, callID: rowStr(ev.Data, "call_id")})
		}
	}
	// the lane that holds a row ("" and true for the whole run), and whether one does
	scope := func(ev Event) (legacyLanePath, bool) {
		if whole {
			return legacyLanePath{}, true
		}
		for _, l := range lanes {
			if l.holds(ev.Path) {
				return l, true
			}
		}
		return legacyLanePath{}, false
	}
	// the op of each call position of the run, by path: the settle step of the old close is read on it
	at := map[string]map[int]string{}
	for _, ev := range events {
		if ev.RunID != runID || ev.Kind != KindCall {
			continue
		}
		if at[ev.Path] == nil {
			at[ev.Path] = map[int]string{}
		}
		at[ev.Path][ev.Position] = ev.Op
	}
	settled := func(ev Event) bool {
		before := at[ev.Path]
		return before[ev.Position-1] == opAudit || (before[ev.Position-1] == opEscalate && before[ev.Position-2] == opAudit)
	}
	// a record row names the run first, then an agent call, then a decision, then an archive: the first of each in the journal's order
	var record, call, decision, archive string
	for _, ev := range events {
		if ev.RunID != runID || ev.Kind != KindCall {
			continue
		}
		switch {
		case archive == "" && ev.Op == OpLegacyArchive:
			lane, in := scope(ev)
			after := settled(ev)
			if !in && !after {
				break
			}
			how := ""
			if after {
				how = " after its crw.audit"
			}
			archive = fmt.Sprintf("its journal holds%s a %s row%s (call %s, milestone %s) of the retired review-thread archive of the close (CRW-1429)", lane.under(), ev.Op, how, ev.CallID, nodeOr(rowStr(ev.Data, "milestone")))
		case record == "" && ev.Op == OpLegacyReview:
			record = fmt.Sprintf("its journal holds a %s row (call %s, node %s) of the retired node reviewer path (CRW-1429)", ev.Op, ev.CallID, nodeOr(rowStr(ev.Data, "node")))
		case record == "" && ev.Op == OpLegacyReviewerFinal:
			record = fmt.Sprintf("its journal holds a %s row (call %s, milestone %s) of the retired reviewer-final path (CRW-1429)", ev.Op, ev.CallID, nodeOr(rowStr(ev.Data, "milestone")))
		case call == "" && ev.Op == "agent" && (rowStr(ev.Data, "role") == legacyReviewerRole || rowStr(ev.Data, "role") == legacyReviewerFinalRole):
			if lane, in := scope(ev); in {
				role := rowStr(ev.Data, "role")
				retired := "the retired node reviewer path"
				if role == legacyReviewerFinalRole {
					retired = "the retired reviewer-final path"
				}
				call = fmt.Sprintf("its journal holds%s a %s agent call (call %s, node %s) of %s (CRW-1429)", lane.under(), role, ev.CallID, nodeOr(rowStr(ev.Data, "node")), retired)
			}
		case decision == "" && ev.Op == opSpawnDecision && ev.Event == EvCompleted && slices.Contains(LegacyRetiredRules, rowStr(ev.Data, "rule")):
			if lane, in := scope(ev); in {
				decision = fmt.Sprintf("its journal holds%s a %s row of the retired rule %s (call %s, node %s) of the retired reviewer paths (CRW-1429)", lane.under(), ev.Op, rowStr(ev.Data, "rule"), ev.CallID, nodeOr(rowStr(ev.Data, "node")))
			}
		}
	}
	for _, why := range []string{record, call, decision, archive} {
		if why != "" {
			return fmt.Sprintf("run %s is a legacy run: %s, which the current project lane does not replay; %s", runID, why, legacyReadOnly)
		}
	}
	return ""
}

// under is where a nested lane's row lies, for the reason: "" for the whole run.
func (p legacyLanePath) under() string {
	if p.id == "" {
		return ""
	}
	return fmt.Sprintf(", under the nested %s (workflow_opened call %s),", p.label, nodeOr(p.callID))
}

// LegacyJournal says why the rows of a run make it a legacy run ("" when nothing in its journal stops it): LegacyIssueChild, then
// LegacyReviewerRun. It is the one journal-only predicate: LegacyRun asks it before its workflow check, and Recover and the recording
// `crw flow audit` ask it before they append.
func LegacyJournal(events []Event, runID string) string {
	if why := LegacyIssueChild(events, runID); why != "" {
		return why
	}
	return LegacyReviewerRun(events, runID)
}

// LegacyRun is LegacyJournal with the workflow the run is resumed or written to as (its spec's, or its script's meta.name): a run that
// names LegacyIssueLaneWorkflow is a legacy `child` session run even when its journal holds no row yet, as the run of an old issue-lane process
// that ended after its spec was written and before its started row (CRW-1415, PR #889 round 2).
func LegacyRun(events []Event, runID, workflow string) string {
	if why := LegacyJournal(events, runID); why != "" {
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

// legacyReadOnly is what the refusal of a legacy run says of it.
const legacyReadOnly = "the run is not resumed and nothing is written to it; its records stay read only (crw flow show and the summary read them)"

func nodeOr(node string) string {
	if node == "" {
		return "?"
	}
	return node
}
