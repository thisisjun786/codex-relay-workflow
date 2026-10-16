package pullrequest

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/flow/engine"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/ops"
)

var (
	headA = strings.Repeat("a", 40)
	headB = strings.Repeat("b", 40)
	headC = strings.Repeat("c", 40)
	headD = strings.Repeat("d", 40)
)

// fixtureRows reads testdata/crosscheck_rows.json, the synthetic crw.crosscheck rows (op, event, data) the readers of this package
// and of ops/report are pinned to, as journal rows numbered in file order.
func fixtureRows(t *testing.T) []engine.Event {
	t.Helper()
	raw, err := os.ReadFile("testdata/crosscheck_rows.json")
	if err != nil {
		t.Fatal(err)
	}
	var list []struct {
		Op    string         `json:"op"`
		Event string         `json:"event"`
		Data  map[string]any `json:"data"`
	}
	if err := json.Unmarshal(raw, &list); err != nil {
		t.Fatal(err)
	}
	var out []engine.Event
	for i, r := range list {
		out = append(out, engine.Event{Seq: i + 1, Kind: engine.KindCall, Op: r.Op, Event: r.Event, Data: r.Data})
	}
	return out
}

// TestCrosscheckOfReadsTheNewestRowAtTheHead pins the accept rule over the shared fixture: the newest completed row of the
// milestone at the head decides, and only a state verdict whose V is verified at that head is accepted.
func TestCrosscheckOfReadsTheNewestRowAtTheHead(t *testing.T) {
	rows := fixtureRows(t)
	for _, c := range []struct {
		name, milestone, head, reason string
	}{
		{"verified at A accepts A", "M1", headA, ""},
		{"verified at A never accepts B", "M1", headB, "crosscheck_head_moved"},
		{"verified at B never accepts the milestone of A", "M3", headA, "crosscheck_head_moved"},
		{"verified at B accepts B", "M3", headB, ""},
		{"a head with no row, rows of the milestone exist", "M1", headC, "crosscheck_head_moved"},
		{"no row of the milestone", "M9", headA, "crosscheck_missing"},
		{"needs_changes then a newer evidence row", "M2", headD, "crosscheck_not_accepted"},
		{"a verified V of another head under the row's head", "M4", headA, "crosscheck_not_accepted"},
		{"a held row", "M5", headA, "crosscheck_not_accepted"},
		{"an evidence row then a newer verified row", "M6", headA, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			got, reason := crosscheckOf(rows, c.milestone, c.head)
			if reason != c.reason {
				t.Fatalf("reason %q, want %q", reason, c.reason)
			}
			if c.reason == "" && (got.Verdict != "verified" || got.Head != c.head) {
				t.Errorf("crosscheck %+v", got)
			}
		})
	}
	got, _ := crosscheckOf(rows, "M1", headA)
	if got.Round != 1 || got.EngineFamily != "gpt" || got.Model != "gpt-6-astra" || got.GuideRev != "crw-crosscheck/1" ||
		len(got.FollowUps) != 1 || !strings.HasPrefix(got.FollowUps[0], "f3 ") {
		t.Errorf("the verified row's members: %+v", got)
	}
	if got, _ := crosscheckOf(rows, "M6", headA); got.Round != 2 {
		t.Errorf("the newer verified row of M6 is round %d, want 2", got.Round)
	}
}

// TestCrosscheckOfIgnoresRowsThatAreNotCompletedCrosscheckCalls: a row of another op, a call row that is not completed, and a flow
// row never count.
func TestCrosscheckOfIgnoresRowsThatAreNotCompletedCrosscheckCalls(t *testing.T) {
	rows := fixtureRows(t)
	verified := rows[0]
	for name, mutate := range map[string]func(*engine.Event){
		"another op":    func(e *engine.Event) { e.Op = "crw.finalGate" },
		"not completed": func(e *engine.Event) { e.Event = engine.EvRefused },
		"a flow row":    func(e *engine.Event) { e.Kind = engine.KindFlow },
	} {
		t.Run(name, func(t *testing.T) {
			e := verified
			mutate(&e)
			if _, reason := crosscheckOf([]engine.Event{e}, "M1", headA); reason != "crosscheck_missing" {
				t.Errorf("reason %q, want crosscheck_missing", reason)
			}
		})
	}
}

// runSpec describes the journal of a run that closed a milestone, as the rows a project lane writes.
type runSpec struct {
	Milestone, Plan string
	// Issues are the issue keys of the milestone's nodes, one node each.
	Issues     []string
	Tip        string
	Gate       string
	Moved      bool
	Crosscheck []engine.Event
	// Status is the finished row's status ("completed"); Outcome its outcome member (state closed, the milestone).
	Status  string
	Outcome map[string]any
	// Resumed appends a started row after the finished row.
	Resumed bool
}

const specGate = `{"head":"%TIP%","state":"pass","evidence":[` +
	`{"kind":"test","command":"go test ./internal/flow/pullrequest/...","state":"fresh","pass":true,"head":"%TIP%","digest":"0123456789abcdef0123","call":"c_aaaaaaaaaaaa","why":"stray"},` +
	`{"kind":"qa","command":"","state":"fresh","pass":true,"head":"%TIP%","digest":"c_0123456789ab","call":"c_bbbbbbbbbbbb"}]}`

func newRunSpec(tip string, issues ...string) runSpec {
	return runSpec{Milestone: "M1", Plan: "plan-1", Issues: issues, Tip: tip, Gate: strings.ReplaceAll(specGate, "%TIP%", tip),
		Status: "completed", Outcome: map[string]any{"state": "closed", "milestone": "M1"}}
}

// crosscheckRow is a completed crw.crosscheck row in the shape of the fixture.
func crosscheckRow(milestone, head, state, verdict string, followUps ...string) engine.Event {
	v := map[string]any{"verdict": verdict, "findings": []any{}, "carried": []any{}, "follow_ups": toAny(followUps), "guide_rev": "crw-crosscheck/1",
		"engine_family": "gpt", "model": "gpt-6-astra", "round": 1, "milestone": milestone, "plan": "plan-1", "head": head}
	return engine.Event{Kind: engine.KindCall, Op: ops.OpCrosscheck, Event: engine.EvCompleted,
		Data: map[string]any{"milestone": milestone, "head": head, "round": 1, "state": state, "verdict": v}}
}

func toAny(ss []string) []any {
	out := []any{}
	for _, s := range ss {
		out = append(out, s)
	}
	return out
}

func jsonString(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// events are the rows of the run in journal order, numbered from 1; a call row's result is a JSON string, as the journal reads it back.
func (s runSpec) events(t *testing.T) []engine.Event {
	t.Helper()
	var nodes []ops.PlanNode
	for i, key := range s.Issues {
		nodes = append(nodes, ops.PlanNode{ID: "n" + string(rune('1'+i)), Issue: key, Title: "t", After: []string{}, Regions: []string{}, Criteria: []string{}})
	}
	ms := ops.Milestone{ID: s.Milestone, Plan: s.Plan, Title: "m", Nodes: nodes}
	closeResult := `{"milestone":"` + s.Milestone + `","tip":"` + s.Tip + `","gate":` + s.Gate + `,"final":null,"audit":null,"gate_head":"` + s.Tip + `","moved":false}`
	rows := []engine.Event{
		{Kind: engine.KindRun, Event: "started", Data: map[string]any{"workflow": "project-lane", "script_digest": "sd", "args_digest": "ad"}},
		{Kind: engine.KindCall, Op: ops.OpMilestone, Event: engine.EvCompleted, Data: map[string]any{"plan": s.Plan, "milestone": s.Milestone, "result": jsonString(t, ms)}},
	}
	rows = append(rows, s.Crosscheck...)
	rows = append(rows, engine.Event{Kind: engine.KindCall, Op: ops.OpMilestoneClose, Event: engine.EvCompleted,
		Data: map[string]any{"milestone": s.Milestone, "plan": s.Plan, "tip": s.Tip, "moved": s.Moved, "result": closeResult}})
	fin := map[string]any{"status": s.Status, "error_kind": ""}
	if s.Outcome != nil {
		fin["outcome"] = s.Outcome
	}
	rows = append(rows, engine.Event{Kind: engine.KindRun, Event: "finished", Data: fin})
	if s.Resumed {
		rows = append(rows, engine.Event{Kind: engine.KindRun, Event: "started", Data: map[string]any{"workflow": "project-lane", "script_digest": "sd", "args_digest": "ad"}})
	}
	for i := range rows {
		rows[i].Seq = i + 1
		rows[i].RunID = "run-1"
	}
	return rows
}

// TestFactsOfReadsAClosedRun pins the run facts over synthetic rows: the last finished row, a resumed run, the milestone and its
// nodes, the close row's tip and moved, and the gate evidence of the close result.
func TestFactsOfReadsAClosedRun(t *testing.T) {
	tip := strings.Repeat("1", 40)
	good := newRunSpec(tip, "o:r:10", "o:r:11")
	good.Crosscheck = []engine.Event{crosscheckRow("M1", tip, "verdict", "verified", "f1 [P3/minor_separable] x.go:2 tidy")}
	f, err := FactsOf(good.events(t))
	if err != nil {
		t.Fatal(err)
	}
	if !f.Closed || f.Why != "" || f.Status != "completed" || f.Outcome == nil || f.Outcome.State != "closed" || f.Plan != "plan-1" || f.Moved {
		t.Fatalf("a closed run: %+v", f)
	}
	b := f.Body
	if b.Milestone != "M1" || b.Tip != tip || b.Gate.State != "pass" || len(b.Gate.Evidence) != 2 || len(f.Nodes) != 2 || f.Nodes[1].Issue != "o:r:11" {
		t.Errorf("facts %+v nodes %+v", b, f.Nodes)
	}
	if e := b.Gate.Evidence[0]; e.Kind != "test" || e.Command != "go test ./internal/flow/pullrequest/..." || e.State != "fresh" || e.Digest != "0123456789abcdef0123" {
		t.Errorf("test evidence %+v", e)
	}
	if c := b.Crosscheck; c.Verdict != "verified" || c.Head != tip || len(c.FollowUps) != 1 {
		t.Errorf("crosscheck %+v", c)
	}

	for name, mutate := range map[string]func(*runSpec){
		"failed status":               func(s *runSpec) { s.Status = "failed" },
		"escalated outcome":           func(s *runSpec) { s.Outcome = map[string]any{"state": "escalated", "milestone": "M1"} },
		"no outcome":                  func(s *runSpec) { s.Outcome = nil },
		"outcome without a milestone": func(s *runSpec) { s.Outcome = map[string]any{"state": "closed"} },
		"a later started row":         func(s *runSpec) { s.Resumed = true },
		"the target moved":            func(s *runSpec) { s.Moved = true },
	} {
		t.Run(name, func(t *testing.T) {
			s := newRunSpec(tip, "o:r:10")
			mutate(&s)
			f, err := FactsOf(s.events(t))
			if err != nil {
				t.Fatal(err)
			}
			if f.Closed || f.Why == "" {
				t.Errorf("closed=%v why=%q", f.Closed, f.Why)
			}
		})
	}
}

// TestFactsOfTakesTheLastRowOfTheMilestoneAndOfTheClose: a replan's later crw.milestone row wins, and so does the later close row.
func TestFactsOfTakesTheLastRowOfTheMilestoneAndOfTheClose(t *testing.T) {
	tip := strings.Repeat("1", 40)
	s := newRunSpec(tip, "o:r:10", "o:r:11")
	rows := s.events(t)
	// an earlier milestone row with one node, and an earlier close row of a target that moved
	first := ops.Milestone{ID: "M1", Plan: "plan-1", Nodes: []ops.PlanNode{{ID: "n1", Issue: "o:r:10"}}}
	early := engine.Event{Kind: engine.KindCall, Op: ops.OpMilestone, Event: engine.EvCompleted, Data: map[string]any{"plan": "plan-1", "milestone": "M1", "result": jsonString(t, first)}}
	earlyClose := engine.Event{Kind: engine.KindCall, Op: ops.OpMilestoneClose, Event: engine.EvCompleted, Data: map[string]any{"milestone": "M1", "tip": "old", "moved": true}}
	all := append([]engine.Event{rows[0], early, earlyClose}, rows[1:]...)
	f, err := FactsOf(all)
	if err != nil {
		t.Fatal(err)
	}
	if !f.Closed || len(f.Nodes) != 2 || f.Body.Tip != tip {
		t.Errorf("the later rows win: closed=%v nodes=%d tip=%q", f.Closed, len(f.Nodes), f.Body.Tip)
	}
	// the later close row says moved: not closed, even with an earlier row that did not
	rows = s.events(t)
	last := rows[len(rows)-2]
	last.Data = map[string]any{"milestone": "M1", "tip": tip, "moved": true}
	moved := append([]engine.Event{}, rows[:len(rows)-2]...)
	moved = append(moved, rows[len(rows)-2], engine.Event{Kind: engine.KindCall, Op: ops.OpMilestoneClose, Event: engine.EvCompleted, Data: last.Data}, rows[len(rows)-1])
	if f, _ := FactsOf(moved); f.Closed {
		t.Errorf("a later close row with moved=true leaves the run closed")
	}
}

// TestFactsOfOfNoRowsIsNotClosed: an empty journal is a run that closed nothing.
func TestFactsOfOfNoRowsIsNotClosed(t *testing.T) {
	f, err := FactsOf(nil)
	if err != nil || f.Closed || f.Why == "" {
		t.Errorf("FactsOf(nil) = %+v, %v", f, err)
	}
}
