package engine

import (
	"context"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/flow/script"
)

// TestLegacyIssueChildPredicate (CRW-1415, PR #889 round 1): a run is a legacy `child` session run when its journal holds a crw.releaseChild
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
		case tc.want != "" && (!strings.Contains(got, "legacy `child` session run") || !strings.Contains(got, tc.want)):
			t.Errorf("%s: %q, want a legacy refusal naming %q", name, got, tc.want)
		}
	}
}

// TestRunRefusesTheResumeOfAnIssueLaneWorkflow (CRW-1415, PR #889 round 2): the resume of a run whose journal holds no row yet (an old
// issue-lane process ended before its started row) is refused as a legacy `child` session run by the workflow it is resumed as, the one the
// config names or the script's meta.name, before a row is written; recover refuses a legacy run before it appends a recovery row.
func TestRunRefusesTheResumeOfAnIssueLaneWorkflow(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	h.cfg.Resume, h.cfg.Workflow = &ResumeOptions{}, LegacyIssueLaneWorkflow
	if _, err := Run(ctx, h.load(`return 1`), h.cfg); err == nil || !strings.Contains(err.Error(), "legacy `child` session run") || !strings.Contains(err.Error(), LegacyIssueLaneWorkflow) {
		t.Errorf("the resume as the issue-lane workflow: %v", err)
	}
	prog, err := script.Load(script.Source{Name: "issue-lane.js", Text: []byte("export const meta = { name: '" + LegacyIssueLaneWorkflow + "' }\nreturn 1\n")})
	if err != nil {
		t.Fatal(err)
	}
	h.cfg.Workflow = ""
	if _, err := Run(ctx, prog, h.cfg); err == nil || !strings.Contains(err.Error(), "legacy `child` session run") {
		t.Errorf("the resume of a script named issue-lane: %v", err)
	}
	if n := len(h.journal.Events()); n != 0 {
		t.Errorf("the refused resumes wrote %d rows", n)
	}

	j := NewMemoryJournal()
	j.Append(Event{RunID: "run-test", Kind: KindRun, Event: "started", Data: map[string]any{"workflow": "project-lane"}})
	j.Append(Event{RunID: "run-test", Kind: KindCall, CallID: "c_child", Op: OpLegacyReleaseChild, Event: EvCompleted, Data: map[string]any{"node": "A"}})
	before := len(j.Events())
	_, err = Recover(ctx, j, ReconcileConfig{RunID: "run-test", Host: &stubHost{}}, RecoverRequest{CallID: "c_child", Absent: true})
	if err == nil || !strings.Contains(err.Error(), "legacy `child` session run") || len(j.Events()) != before {
		t.Errorf("recover of a legacy run: %v, rows %d -> %d", err, before, len(j.Events()))
	}
}

// The rows of the old reviewer paths (CRW-1429): helpers of the predicate tests below.
func startedRow(run, workflow string) Event {
	return Event{RunID: run, Kind: KindRun, Event: "started", Data: map[string]any{"workflow": workflow}}
}

func opRow(run, callID, path string, position int, op string, data map[string]any) Event {
	return Event{RunID: run, Kind: KindCall, CallID: callID, Path: path, Position: position, Op: op, Label: op, Event: EvCompleted, Data: data}
}

func agentRow(run, callID, path string, position int, role, node string) Event {
	return Event{RunID: run, Kind: KindCall, CallID: callID, Path: path, Position: position, Op: "agent", Label: role + ":" + node, Event: EvPrepared, Data: map[string]any{"role": role, "node": node}}
}

func openedRow(run, path string, position int, label, callID string) Event {
	return Event{RunID: run, Kind: KindFlow, Path: path, Position: position, Op: "workflow", Label: label, Event: "workflow_opened", Data: map[string]any{"call_id": callID, "script_digest": "d"}}
}

// TestLegacyReviewerRunNamesTheRetiredRow (CRW-1429): a run whose journal holds a row of the retired node reviewer or reviewer-final path
// is a legacy run, and the reason names the row (its op or role, its call id, its node or milestone), the retired path and CRW-1429.
// A crw.review or crw.reviewerFinal row is one in any run; a reviewer or reviewer-final agent call and a spawn decision of a retired
// rule are one only where an old lane wrote them: the whole run when it was started as a lane workflow, else the subtree of a lane it
// nested with workflow() (by name or by {scriptPath}), whose reason also names the nested label and its workflow_opened call. A crw.archive
// row is one in the lane scope, and anywhere in the run when it follows a crw.audit on its path (CRW-1447: the old close's settle step,
// with or without its audit_mismatch escalation between).
func TestLegacyReviewerRunNamesTheRetiredRow(t *testing.T) {
	const run = "run-1"
	nested := nestedPathID(RootPath, 3)
	for name, tc := range map[string]struct {
		events []Event
		want   []string
	}{
		"a crw.review row": {[]Event{startedRow(run, "project-lane"), opRow(run, "c_rev", RootPath, 7, OpLegacyReview, map[string]any{"node": "A", "call": "c_r"})},
			[]string{OpLegacyReview, "c_rev", "node A", "the retired node reviewer path (CRW-1429)", "which the current project lane does not replay", legacyReadOnly}},
		"a crw.review row of a custom run": {[]Event{startedRow(run, "custom"), opRow(run, "c_rev", RootPath, 2, OpLegacyReview, map[string]any{"node": "B"})},
			[]string{OpLegacyReview, "c_rev", "node B", "CRW-1429"}},
		"a crw.reviewerFinal row": {[]Event{startedRow(run, "custom"), opRow(run, "c_revf", RootPath, 4, OpLegacyReviewerFinal, map[string]any{"milestone": "M1"})},
			[]string{OpLegacyReviewerFinal, "c_revf", "milestone M1", "the retired reviewer-final path (CRW-1429)"}},
		"a reviewer agent call of the project lane, before its crw.review row": {[]Event{startedRow(run, "project-lane"), agentRow(run, "c_r", RootPath, 7, "reviewer", "A")},
			[]string{"reviewer agent call", "c_r", "node A", "the retired node reviewer path (CRW-1429)"}},
		"a reviewer-final agent call of the land-head workflow": {[]Event{startedRow(run, "land-head"), agentRow(run, "c_rf", RootPath, 9, "reviewer-final", "")},
			[]string{"reviewer-final agent call", "c_rf", "the retired reviewer-final path (CRW-1429)"}},
		"a decision of a retired rule": {[]Event{startedRow(run, "night-batch"), opRow(run, "c_sp", RootPath, 6, opSpawnDecision, map[string]any{"rule": "SP-REV-1", "node": "A", "role": "reviewer"})},
			[]string{opSpawnDecision, "SP-REV-1", "c_sp", "node A", "CRW-1429"}},
		"the decision and the reviewer call under a nested project lane": {[]Event{startedRow(run, "custom"), openedRow(run, RootPath, 3, "workflow:project-lane", "c_wf"),
			opRow(run, "c_sp", nested, 12, opSpawnDecision, map[string]any{"rule": "SP-REV-1", "node": "A", "role": "reviewer"}),
			agentRow(run, "c_r", nested, 13, "reviewer", "A")},
			[]string{"reviewer agent call", "c_r", "node A", "workflow:project-lane", "c_wf", "CRW-1429"}},
		"a reviewer call under a lane nested by its script path": {[]Event{startedRow(run, "custom"), openedRow(run, RootPath, 3, "workflow:lanes/project-lane.js", "c_wf"),
			agentRow(run, "c_r", nested+"/4:A", 1, "reviewer", "A")},
			[]string{"reviewer agent call", "c_r", "workflow:lanes/project-lane.js", "c_wf"}},
		"a decision of the retired evidence explorer under a nested night batch": {[]Event{startedRow(run, "custom"), openedRow(run, RootPath, 3, "workflow:night-batch", "c_wf"),
			opRow(run, "c_sp", nested, 2, opSpawnDecision, map[string]any{"rule": "SP-EXPL-3", "node": "A"})},
			[]string{"SP-EXPL-3", "c_sp", "workflow:night-batch"}},
		"a crw.archive row of the project lane": {[]Event{startedRow(run, "project-lane"), opRow(run, "c_arc", RootPath, 21, OpLegacyArchive, map[string]any{"milestone": "M1"})},
			[]string{OpLegacyArchive, "c_arc", "milestone M1", "the retired review-thread archive of the close (CRW-1429)", "which the current project lane does not replay", legacyReadOnly}},
		"a crw.archive row under a nested project lane": {[]Event{startedRow(run, "custom"), openedRow(run, RootPath, 3, "workflow:project-lane", "c_wf"),
			opRow(run, "c_arc", nested, 40, OpLegacyArchive, map[string]any{"milestone": "M1"})},
			[]string{OpLegacyArchive, "c_arc", "milestone M1", "workflow:project-lane", "c_wf", "the retired review-thread archive of the close (CRW-1429)"}},
		"the settle shape of a custom run: the audit, then the archive": {[]Event{startedRow(run, "custom"),
			opRow(run, "c_aud", RootPath, 20, "crw.audit", map[string]any{"milestone": "M1"}), opRow(run, "c_arc", RootPath, 21, OpLegacyArchive, map[string]any{"milestone": "M1"})},
			[]string{OpLegacyArchive, "c_arc", "milestone M1", "after its crw.audit", "the retired review-thread archive of the close (CRW-1429)"}},
		"the settle shape with the audit_mismatch escalation between": {[]Event{startedRow(run, "custom"),
			opRow(run, "c_aud", RootPath, 20, "crw.audit", map[string]any{"milestone": "M1"}), opRow(run, "c_esc", RootPath, 21, "crw.escalate", map[string]any{"reason": "audit_mismatch"}),
			opRow(run, "c_arc", RootPath, 22, OpLegacyArchive, map[string]any{"milestone": "M1"})},
			[]string{OpLegacyArchive, "c_arc", "after its crw.audit", "CRW-1429"}},
		"the settle shape under a lane nested by a script path of another name": {[]Event{startedRow(run, "custom"), openedRow(run, RootPath, 3, "workflow:lanes/my-lane.js", "c_wf"),
			opRow(run, "c_aud", nested, 30, "crw.audit", map[string]any{"milestone": "M1"}), opRow(run, "c_arc", nested, 31, OpLegacyArchive, map[string]any{"milestone": "M1"}),
			opRow(run, "c_up", nested, 32, "crw.reportUp", map[string]any{"milestone": "M1"})},
			[]string{OpLegacyArchive, "c_arc", "milestone M1", "after its crw.audit", "CRW-1429"}},
	} {
		got := LegacyReviewerRun(tc.events, run)
		for _, w := range tc.want {
			if !strings.Contains(got, w) {
				t.Errorf("%s: %q does not name %q", name, got, w)
			}
		}
		if j := LegacyJournal(tc.events, run); j != got {
			t.Errorf("%s: LegacyJournal %q, LegacyReviewerRun %q", name, j, got)
		}
		if r := LegacyRun(tc.events, run, "custom"); r != got {
			t.Errorf("%s: LegacyRun %q, LegacyReviewerRun %q", name, r, got)
		}
	}
	// a legacy `child` session run is named as one first
	both := []Event{startedRow(run, "project-lane"), opRow(run, "c_child", RootPath, 2, OpLegacyReleaseChild, map[string]any{"node": "A"}), opRow(run, "c_rev", RootPath, 3, OpLegacyReview, map[string]any{"node": "B"})}
	if got := LegacyJournal(both, run); !strings.Contains(got, "legacy `child` session run") {
		t.Errorf("a run with both: %q", got)
	}
}

// TestCurrentLaneRunIsNotTakenForALegacyReviewerRun (CRW-1429): what the current lane writes is no legacy row (the unit check, the
// critic, the close's audit, its audit_mismatch escalation and its report), and neither is a reviewer call or a crw.archive of a user
// workflow outside any nested lane that does not follow a crw.audit on its path (right after it, or after it and an escalation), a
// reviewer call under a nested workflow that is not a lane, or a row of another run.
func TestCurrentLaneRunIsNotTakenForALegacyReviewerRun(t *testing.T) {
	const run = "run-1"
	for name, events := range map[string][]Event{
		"the current project lane": {startedRow(run, "project-lane"), agentRow(run, "c_crit", RootPath, 3, "critic", ""),
			opRow(run, "c_sp", RootPath, 4, opSpawnDecision, map[string]any{"rule": "SP-AUD-1", "role": "critic"}),
			opRow(run, "c_uc", RootPath+"/9:A", 5, "crw.unitCheck", map[string]any{"node": "A", "role": "unit-check"}),
			opRow(run, "c_aud", RootPath, 20, "crw.audit", map[string]any{"milestone": "M1"}),
			opRow(run, "c_esc", RootPath, 21, "crw.escalate", map[string]any{"reason": "audit_mismatch"}),
			opRow(run, "c_up", RootPath, 22, "crw.reportUp", map[string]any{"milestone": "M1"})},
		"a custom whose archive follows another op after its audit": {startedRow(run, "custom"),
			opRow(run, "c_aud", RootPath, 20, "crw.audit", map[string]any{"milestone": "M1"}), opRow(run, "c_up", RootPath, 21, "crw.reportUp", map[string]any{"milestone": "M1"}),
			opRow(run, "c_arc", RootPath, 22, OpLegacyArchive, map[string]any{"milestone": "M1"})},
		"a custom whose archive follows an audit on another path": {startedRow(run, "custom"),
			opRow(run, "c_aud", RootPath+"/2:a", 4, "crw.audit", map[string]any{"milestone": "M1"}), opRow(run, "c_arc", RootPath, 5, OpLegacyArchive, map[string]any{"milestone": "M1"})},
		"a custom whose archive follows an escalation with no audit before it": {startedRow(run, "custom"),
			opRow(run, "c_x", RootPath, 19, "crw.hold", map[string]any{"node": "A"}), opRow(run, "c_esc", RootPath, 20, "crw.escalate", map[string]any{"reason": "audit_mismatch"}),
			opRow(run, "c_arc", RootPath, 21, OpLegacyArchive, map[string]any{"milestone": "M1"})},
		"an archive after its audit in another run": {startedRow(run, "custom"), startedRow("run-2", "custom"),
			opRow("run-2", "c_aud", RootPath, 20, "crw.audit", map[string]any{"milestone": "M1"}), opRow("run-2", "c_arc", RootPath, 21, OpLegacyArchive, map[string]any{"milestone": "M1"})},
		"a custom with a direct archive and a reviewer call": {startedRow(run, "custom"), agentRow(run, "c_r", RootPath, 1, "reviewer", "A"),
			opRow(run, "c_sp", RootPath, 2, opSpawnDecision, map[string]any{"rule": "SP-REV-1", "role": "reviewer"}),
			opRow(run, "c_arc", RootPath, 3, OpLegacyArchive, map[string]any{"milestone": "M1"})},
		"a reviewer call under a nested helper": {startedRow(run, "custom"), openedRow(run, RootPath, 3, "workflow:helper", "c_wf"),
			agentRow(run, "c_r", nestedPathID(RootPath, 3), 1, "reviewer", "A")},
		"a reviewer call under a helper nested by its script path": {startedRow(run, "custom"), openedRow(run, RootPath, 3, "workflow:lanes/helper.js", "c_wf"),
			agentRow(run, "c_r", nestedPathID(RootPath, 3), 1, "reviewer-final", "")},
		"a reviewer call beside a nested lane, on a path the lane's is a prefix of": {startedRow(run, "custom"), openedRow(run, RootPath, 3, "workflow:project-lane", "c_wf"),
			openedRow(run, RootPath, 30, "workflow:helper", "c_wf2"), agentRow(run, "c_r", nestedPathID(RootPath, 30), 1, "reviewer", "A")},
		"another run's rows": {startedRow(run, "project-lane"), startedRow("run-2", "project-lane"),
			opRow("run-2", "c_rev", RootPath, 2, OpLegacyReview, map[string]any{"node": "A"}), agentRow("run-2", "c_r", RootPath, 3, "reviewer", "A")},
	} {
		if got := LegacyJournal(events, run); got != "" {
			t.Errorf("%s: refused %q", name, got)
		}
	}
}

// TestNestedPathIDIsTheEnginesChildPath (CRW-1429): the lane scope of a nested workflow reads its rows by nestedPathID, so the engine
// writes the rows of a workflow() child under nestedPathID(the workflow_opened row's path, its position), and a join item's path is
// still childID's.
func TestNestedPathIDIsTheEnginesChildPath(t *testing.T) {
	h := newHarness(t)
	child, err := script.Load(script.Source{Name: "kid.js", Text: []byte("export const meta = { name: 'kid' }\nconst r = await agent.call('child work', { agentType: 'explorer', label: 'child-call' })\nawait parallel([async () => agent.call('item', { agentType: 'explorer', label: 'item-call' })], { keys: ['x'] })\nreturn r\n")})
	if err != nil {
		t.Fatal(err)
	}
	h.cfg.Workflows = func(string, string) (*script.Program, error) { return child, nil }
	h.run("await agent.call('first', { agentType: 'explorer', label: 'first' })\nreturn await workflow('kid')")
	var open *Event
	for _, ev := range h.journal.Events() {
		if ev.Event == "workflow_opened" {
			ev := ev
			open = &ev
		}
	}
	if open == nil {
		t.Fatal("no workflow_opened row")
	}
	nested := nestedPathID(open.Path, open.Position)
	if want := (&pathState{id: open.Path}).childID(open.Position, "workflow"); nested != want {
		t.Errorf("nestedPathID %q, childID %q", nested, want)
	}
	paths := map[string]string{}
	for _, ev := range h.journal.Events() {
		if ev.Kind == KindCall && ev.Op == "agent" && ev.Event == EvPrepared {
			paths[ev.Label] = ev.Path
		}
	}
	if paths["child-call"] != nested || paths["item-call"] != (&pathState{id: nested}).childID(2, "x") || paths["first"] != RootPath {
		t.Errorf("the child's rows are on %v, nestedPathID is %q", paths, nested)
	}
}
