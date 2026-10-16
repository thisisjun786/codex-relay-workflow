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
