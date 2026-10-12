package shadow_test

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/flow/engine"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/shadow"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/trace"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/trace/tracetest"
)

const dataDir = "../../../testdata/flow/shadow"

// scenarioTrace is the trace of the synthetic store copy.
func scenarioTrace(t *testing.T) (*tracetest.Scenario, string, *trace.Trace) {
	t.Helper()
	s := tracetest.NewScenario(tracetest.New(t))
	path := s.Copy()
	tr, err := trace.Export(tracetest.Context(t), trace.Options{Path: path})
	if err != nil {
		t.Fatal(err)
	}
	return s, path, tr
}

// compare replays the script file name of testdata/flow/shadow on the trace.
func compare(t *testing.T, tr *trace.Trace, name string) *shadow.Report {
	t.Helper()
	text, err := os.ReadFile(filepath.Join(dataDir, name))
	if err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	rep, err := shadow.Compare(tracetest.Context(t), tr, shadow.Config{Script: text, Name: name, Dir: root.FS(), Policy: engine.FakePolicy()})
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return rep
}

func counts(rep *shadow.Report) map[string]int {
	out := map[string]int{}
	for c, n := range rep.Counts {
		if n != 0 {
			out[c] = n
		}
	}
	return out
}

// TestShadowClassifiesMismatch replays workflow scripts over the recorded run of a synthetic store copy and classifies every
// decision pair in the closed set: the recorded script itself is all same (the unknown outcome of n3 replays as the same
// escalation), a script that reviews before it builds is different_order, one more spawn is extra_spawn, one fewer is
// missing_spawn (and the escalation it ended in becomes different_escalation), a build on the higher tier is different_tier. No
// agent is called: every call the recorded run holds is answered from the trace.
func TestShadowClassifiesMismatch(t *testing.T) {
	_, _, tr := scenarioTrace(t)
	recorded, err := os.ReadFile(filepath.Join(dataDir, "recorded.js"))
	if err != nil {
		t.Fatal(err)
	}
	if string(recorded) != tracetest.RecordedScript {
		t.Fatalf("testdata/flow/shadow/recorded.js is not tracetest.RecordedScript")
	}
	for _, tc := range []struct {
		script   string
		want     map[string]int
		invented int
		check    func(t *testing.T, rep *shadow.Report)
	}{
		{"recorded.js", map[string]int{"same": 7}, 0, nil},
		{"order.js", map[string]int{"same": 5, "different_order": 2}, 0, func(t *testing.T, rep *shadow.Report) {
			for _, it := range rep.Runs[0].Items {
				if it.Class == shadow.ClassDifferentOrder && it.Node != "n1" {
					t.Errorf("different_order on node %q", it.Node)
				}
			}
		}},
		{"extra.js", map[string]int{"same": 7, "extra_spawn": 1}, 1, func(t *testing.T, rep *shadow.Report) {
			for _, it := range rep.Runs[0].Items {
				if it.Class == shadow.ClassExtraSpawn && (it.Actual != nil || it.Computed == nil || it.Computed.Role != "explorer") {
					t.Errorf("extra_spawn item = %+v", it)
				}
			}
		}},
		{"missing.js", map[string]int{"same": 5, "missing_spawn": 1, "different_escalation": 1}, 0, func(t *testing.T, rep *shadow.Report) {
			for _, it := range rep.Runs[0].Items {
				if it.Class == shadow.ClassMissingSpawn && (it.Computed != nil || it.Actual == nil || it.Actual.Node != "n3") {
					t.Errorf("missing_spawn item = %+v", it)
				}
			}
		}},
		{"tier.js", map[string]int{"same": 6, "different_tier": 1}, 0, func(t *testing.T, rep *shadow.Report) {
			for _, it := range rep.Runs[0].Items {
				if it.Class == shadow.ClassDifferentTier && (it.Actual.Role != "executor" || it.Computed.Role != "executor-t1" || it.Node != "n3") {
					t.Errorf("different_tier item = %+v", it)
				}
			}
		}},
	} {
		t.Run(tc.script, func(t *testing.T) {
			rep := compare(t, tr, tc.script)
			if got := counts(rep); !mapsEqual(got, tc.want) {
				t.Errorf("counts = %v, want %v\n%s", got, tc.want, dump(rep))
			}
			run := rep.Runs[0]
			if run.Replay.Status != engine.RunCompleted || run.Replay.Invented != tc.invented {
				t.Errorf("replay = %+v, want completed with %d invented", run.Replay, tc.invented)
			}
			if run.Replay.Replayed == 0 {
				t.Errorf("nothing was answered from the trace")
			}
			total := 0
			for _, c := range shadow.Classes {
				if _, ok := rep.Counts[c]; !ok {
					t.Errorf("the report has no count for %s", c)
				}
				total += rep.Counts[c]
			}
			if total != len(run.Items) || len(rep.Counts) != len(shadow.Classes) {
				t.Errorf("counts %v do not cover the %d items", rep.Counts, len(run.Items))
			}
			for _, it := range run.Items {
				if !slices.Contains(shadow.Classes, it.Class) {
					t.Errorf("class %q is not in the closed set", it.Class)
				}
			}
			if tc.check != nil {
				tc.check(t, rep)
			}
		})
	}
	// the argument digest of the recorded run is the one of no args
	if rep := compare(t, tr, "recorded.js"); !rep.Runs[0].ArgsMatch || rep.Runs[0].RecordedScriptDigest == "" {
		t.Errorf("run report = %+v", rep.Runs[0])
	}
}

func mapsEqual(a, b map[string]int) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

func dump(rep *shadow.Report) string {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetIndent("", " ")
	_ = enc.Encode(rep.Runs[0].Items)
	return b.String()
}

// TestClassifyEdges pins the rules of Classify on decisions given directly: a pair whose roles, pairs or reasons differ, a
// decision on one side only, the order of the decisions of a node, and the order of calls with no node (not compared).
func TestClassifyEdges(t *testing.T) {
	sp := func(order int, node, role, family, label string) shadow.Decision {
		return shadow.Decision{Kind: shadow.KindSpawn, Node: node, Role: role, Family: family, Label: label, Pair: role + "/x", Order: order}
	}
	class := func(items []shadow.Item) []string {
		var out []string
		for _, it := range items {
			out = append(out, it.Kind+":"+it.Class)
		}
		return out
	}
	// a pair on another model is a tier difference even with the same role
	a := sp(1, "n", "executor", "executor", "")
	c := a
	c.Pair = "other/x"
	if got := class(shadow.Classify([]shadow.Decision{a}, []shadow.Decision{c})); !slices.Equal(got, []string{"spawn:different_tier"}) {
		t.Errorf("pair: %v", got)
	}
	// escalations: same reason is same, another reason is different_escalation, one side only is different_escalation
	e1 := shadow.Decision{Kind: shadow.KindEscalation, Node: "n", Reason: "dispatch_unknown", Order: 1}
	e2 := e1
	e2.Reason = "restore_snapshot_mismatch"
	if got := class(shadow.Classify([]shadow.Decision{e1}, []shadow.Decision{e1})); !slices.Equal(got, []string{"escalation:same"}) {
		t.Errorf("same escalation: %v", got)
	}
	if got := class(shadow.Classify([]shadow.Decision{e1}, []shadow.Decision{e2})); !slices.Equal(got, []string{"escalation:different_escalation"}) {
		t.Errorf("escalation reason: %v", got)
	}
	if got := class(shadow.Classify(nil, []shadow.Decision{e2})); !slices.Equal(got, []string{"escalation:different_escalation"}) {
		t.Errorf("escalation of the script only: %v", got)
	}
	// tier steps: one side only is different_tier, another target is different_tier
	t1 := shadow.Decision{Kind: shadow.KindTier, Node: "n", Role: "executor-t1", Order: 1}
	t2 := t1
	t2.Role = "architect"
	if got := class(shadow.Classify([]shadow.Decision{t1}, nil)); !slices.Equal(got, []string{"tier:different_tier"}) {
		t.Errorf("tier step of the run only: %v", got)
	}
	if got := class(shadow.Classify([]shadow.Decision{t1}, []shadow.Decision{t2})); !slices.Equal(got, []string{"tier:different_tier"}) {
		t.Errorf("tier target: %v", got)
	}
	// flushes are spawns of a reviewer: one side only is extra_spawn or missing_spawn
	f := shadow.Decision{Kind: shadow.KindFlush, Role: "reviewer", Family: "reviewer", Order: 1}
	if got := class(shadow.Classify(nil, []shadow.Decision{f})); !slices.Equal(got, []string{"flush:extra_spawn"}) {
		t.Errorf("flush of the script only: %v", got)
	}
	if got := class(shadow.Classify([]shadow.Decision{f}, nil)); !slices.Equal(got, []string{"flush:missing_spawn"}) {
		t.Errorf("flush of the run only: %v", got)
	}
	// the nth spawn of a node is paired with the nth: a second build the script lacks is missing_spawn
	b1, b2 := sp(1, "n", "executor", "executor", ""), sp(2, "n", "executor", "executor", "")
	if got := class(shadow.Classify([]shadow.Decision{b1, b2}, []shadow.Decision{b1})); !slices.Equal(got, []string{"spawn:same", "spawn:missing_spawn"}) {
		t.Errorf("second build: %v", got)
	}
	// order within a node is compared, the order of calls with no node is not
	r1, r2 := sp(1, "n", "executor", "executor", ""), sp(2, "n", "reviewer", "reviewer", "")
	r1c, r2c := r1, r2
	r1c.Order, r2c.Order = 2, 1
	if got := class(shadow.Classify([]shadow.Decision{r1, r2}, []shadow.Decision{r2c, r1c})); !slices.Equal(got, []string{"spawn:different_order", "spawn:different_order"}) {
		t.Errorf("node order: %v", got)
	}
	x1, x2 := sp(1, "", "explorer", "explorer", "a"), sp(2, "", "explorer", "explorer", "b")
	x1c, x2c := x1, x2
	x1c.Order, x2c.Order = 2, 1
	if got := class(shadow.Classify([]shadow.Decision{x1, x2}, []shadow.Decision{x2c, x1c})); !slices.Equal(got, []string{"spawn:same", "spawn:same"}) {
		t.Errorf("nodeless order: %v", got)
	}
}

// TestShadowFlushDecisions replays a run whose reviewer call reviews a batch: the call is a flush, not a spawn, whichever
// side it is on; a script that does not review the batch misses the flush, and a flush the run recorded in a decision row of its own
// is counted with the journal's flush, not twice.
func TestShadowFlushDecisions(t *testing.T) {
	text, err := os.ReadFile(filepath.Join(dataDir, "batch.js"))
	if err != nil {
		t.Fatal(err)
	}
	b := tracetest.New(t)
	b.Run("run-b", "plan-b", string(text), nil, nil)
	tr, err := trace.Export(tracetest.Context(t), trace.Options{Path: b.Copy()})
	if err != nil {
		t.Fatal(err)
	}
	if rep := compare(t, tr, "batch.js"); !mapsEqual(counts(rep), map[string]int{"same": 1}) || rep.Runs[0].Items[0].Kind != shadow.KindFlush {
		t.Errorf("batch.js: %v\n%s", counts(rep), dump(rep))
	}
	nothing := shadow.Config{Script: []byte("export const meta = { name: 'none' }\nreturn 1\n"), Name: "none.js", Policy: engine.FakePolicy()}
	rep, err := shadow.Compare(tracetest.Context(t), tr, nothing)
	if err != nil {
		t.Fatal(err)
	}
	if got := counts(rep); !mapsEqual(got, map[string]int{"missing_spawn": 1}) || rep.Runs[0].Items[0].Kind != shadow.KindFlush {
		t.Errorf("empty script: %v\n%s", got, dump(rep))
	}
	// a recorded flush row of the call the journal already shows is the same flush; a second row is one more
	row := trace.Decision{Source: "flow_spawn_decisions", ID: "sd-x", Kind: "flush", Rule: "SP-REV-2", Decision: "flush", At: tracetest.At(1)}
	tr.Runs[0].Decisions = append(tr.Runs[0].Decisions, row)
	rep, err = shadow.Compare(tracetest.Context(t), tr, nothing)
	if err != nil {
		t.Fatal(err)
	}
	if got := counts(rep); !mapsEqual(got, map[string]int{"missing_spawn": 1}) {
		t.Errorf("one recorded flush row: %v", got)
	}
	row.ID = "sd-y"
	tr.Runs[0].Decisions = append(tr.Runs[0].Decisions, row)
	rep, err = shadow.Compare(tracetest.Context(t), tr, nothing)
	if err != nil {
		t.Fatal(err)
	}
	if got := counts(rep); !mapsEqual(got, map[string]int{"missing_spawn": 2}) {
		t.Errorf("two recorded flush rows: %v", got)
	}
}

// TestShadowReplaysRecordedFailures answers a failed call as the host did: the unknown outcome of n3 and a held call replay
// into the same terminal statuses, so the escalation is computed, not copied.
func TestShadowReplaysRecordedFailures(t *testing.T) {
	b := tracetest.New(t)
	fake := engine.NewFake()
	fake.Handler = func(r engine.FakeRequest) engine.FakeReply {
		if strings.Contains(r.Prompt, "held") {
			return engine.FakeReply{Fail: &engine.TurnFailure{CodexErrorInfo: "serverOverloaded", Message: "busy"}}
		}
		return engine.FakeReply{}
	}
	src := "export const meta = { name: 'f' }\nawait agent.call('held build', { agentType: 'executor', node: 'n1', label: 'held' })\nawait agent.call('ok build', { agentType: 'executor', node: 'n1', label: 'ok' })\nreturn 1\n"
	b.Run("run-f", "plan-f", src, fake, nil)
	tr, err := trace.Export(tracetest.Context(t), trace.Options{Path: b.Copy()})
	if err != nil {
		t.Fatal(err)
	}
	var held trace.Call
	for _, c := range tr.Runs[0].Calls {
		if c.Label == "held" {
			held = c
		}
	}
	if held.Status != "held" || held.Reason != "host_infra_serverOverloaded" {
		t.Fatalf("recorded call = %+v", held)
	}
	rep, err := shadow.Compare(tracetest.Context(t), tr, shadow.Config{Script: []byte(src), Name: "f.js", Policy: engine.FakePolicy()})
	if err != nil {
		t.Fatal(err)
	}
	if got := counts(rep); !mapsEqual(got, map[string]int{"same": 2}) || rep.Runs[0].Replay.Invented != 0 {
		t.Errorf("counts %v, replay %+v", got, rep.Runs[0].Replay)
	}
}

// TestDeriveCountsACallOnce reads the decisions of a journal in which a resumed run prepared a call again: one spawn, the first
// row's. A call that continues a handle is no spawn, and a tier step takes its node from the call that carries its step id.
func TestDeriveCountsACallOnce(t *testing.T) {
	prepared := func(seq int, call, role, node string, extra map[string]any) engine.Event {
		data := map[string]any{"role": role, "family": role, "node": node, "model": "m", "effort": "e"}
		for k, v := range extra {
			data[k] = v
		}
		return engine.Event{Seq: seq, Kind: engine.KindCall, CallID: call, Op: "agent", Event: engine.EvPrepared, Data: data}
	}
	events := []engine.Event{
		prepared(1, "c_1", "executor", "n1", nil),
		prepared(2, "c_1", "executor", "n1", nil), // the resumed run prepares it again
		prepared(3, "c_2", "executor", "n1", map[string]any{"resume_of": "c_1"}),
		{Seq: 4, Kind: engine.KindCall, CallID: "c_3", Op: "crw.tierStep", Event: engine.EvCompleted, Data: map[string]any{"step_decision": "step_3"}},
		prepared(5, "c_4", "executor-t1", "n2", map[string]any{"step_decision": "step_3"}),
	}
	got := shadow.Derive(events, nil)
	var kinds []string
	for _, d := range got {
		kinds = append(kinds, d.Kind+":"+d.Node+":"+d.Role)
	}
	if want := []string{"spawn:n1:executor", "tier:n2:executor-t1", "spawn:n2:executor-t1"}; !slices.Equal(kinds, want) {
		t.Errorf("decisions = %v, want %v", kinds, want)
	}
}
