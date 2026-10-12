package manage

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	flowcli "github.com/thisisjun786/codex-relay-workflow/internal/flow/cli"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/engine"
)

// CRW-1205: the morning report of a night batch is made from the run digest `crw flow digest` builds from the run's journal, in the
// form of the 1040a report. The digest here is built from the rows of a run the way the command builds it, and goes through the same
// JSON file the two commands share.

func morningRow(kind, event, call, op string, data map[string]any) engine.Event {
	return engine.Event{RunID: "run-night", Kind: kind, Event: event, CallID: call, Op: op, Data: data}
}

// TestMorningReportFromDigest: A landed with its commit, acceptance and integration in the journal, B's executor was held when the
// token budget was spent, and CRW-C was selected but is no node of the run's plan. The report puts A under "통합됨" with the journal's
// evidence, B and C under "취소·이월" with their reasons, names the budget as why the run stopped, and claims nothing the journal does
// not show: the providers are not observed.
func TestMorningReportFromDigest(t *testing.T) {
	milestone := `{"id":"M1","plan":"plan-1","title":"night","nodes":[{"id":"A","issue":"CRW-A","title":"issue A"},{"id":"B","issue":"CRW-B","title":"issue B"}]}`
	events := []engine.Event{
		morningRow(engine.KindRun, "started", "", "", map[string]any{"workflow": "night-batch"}),
		morningRow(engine.KindCall, engine.EvCompleted, "c_ms", "crw.milestone", map[string]any{"result": milestone}),
		morningRow(engine.KindCall, engine.EvPrepared, "c_a", "agent", map[string]any{"role": "executor", "node": "A", "attempt": 1}),
		morningRow(engine.KindCall, engine.EvCompleted, "c_a", "agent", map[string]any{"tokens": map[string]any{"total": float64(20)}}),
		morningRow(engine.KindCall, engine.EvCompleted, "c_commit", "crw.commit", map[string]any{"node": "A", "commit": "aaaa1111bbbb2222"}),
		morningRow(engine.KindCall, engine.EvCompleted, "c_accept", "crw.accept", map[string]any{"node": "A", "acceptance_id": "acc-A"}),
		morningRow(engine.KindCall, engine.EvCompleted, "c_integrate", "crw.integrate", map[string]any{"node": "A", "batch_commit": "cccc3333dddd4444"}),
		morningRow(engine.KindCall, engine.EvPrepared, "c_b", "agent", map[string]any{"role": "executor", "node": "B", "attempt": 1}),
		morningRow(engine.KindCall, engine.EvHeld, "c_b", "agent", map[string]any{"reason": engine.ReasonBudgetExhausted, "input_state": "not_delivered"}),
		morningRow(engine.KindRun, "finished", "", "", map[string]any{"status": engine.RunCompleted}),
	}
	for i := range events {
		events[i].Seq = i + 1
	}
	digest := flowcli.BuildDigest(flowcli.DigestInput{
		Run: flowcli.RunRecord{RunID: "run-night", Workflow: "night-batch", StartedAt: "2026-10-12T21:00:00Z"}, Events: events,
		Spec: flowcli.Spec{TokenBudget: 25, Deadline: "2026-10-13T06:00:00Z"}, Result: &flowcli.Result{Run: "run-night", EndedAt: "2026-10-12T21:40:00Z"},
		Label: "night-2026-10-12", Issues: []string{"CRW-A", "CRW-B", "CRW-C"},
	})
	// the file the two commands share
	raw, err := json.Marshal(digest)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := batchParseDigest(raw)
	if err != nil {
		t.Fatalf("crw manage batch report refuses the digest crw flow digest builds: %v\n%s", err, raw)
	}
	report := BatchReport(parsed, time.Date(2026, 10, 13, 6, 30, 0, 0, time.UTC))
	for _, want := range []string{
		"# 밤 배치 아침 보고: night-2026-10-12", "선택 3건 중 통합 1건, 취소 0건, 이월 2건, Jun 결정 필요 0건, 미확인 0건", "기한 2026-10-13T06:00:00Z", "종료 사유 token budget reached (20 of 25 tokens)",
		"## 통합됨\n\n- CRW-A issue A: 증거 run run-night node A: commit aaaa1111bbbb22, acceptance acc-A, integrated cccc3333dddd44", "- CRW-B issue B: 이월: the node's work is held: budget_exhausted",
		"- CRW-C: 이월: the run did not reach the plan that holds the issue", "종료: 2026-10-12T21:40:00Z", "제공자 관측: model providers: 관측 안 됨",
	} {
		if !strings.Contains(report, want) {
			t.Errorf("the report lacks %q:\n%s", want, report)
		}
	}
	if strings.Contains(report, "CRW-B issue B: 증거") || strings.Contains(report, "CRW-C: 증거") {
		t.Errorf("the report claims a landing the journal does not show:\n%s", report)
	}
}
