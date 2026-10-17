package ops

import (
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/flow/engine"
)

func verifyRow(call, head, digest, record string) engine.Event {
	return engine.Event{Seq: 1, Kind: engine.KindCall, Op: OpVerify, CallID: call, Event: engine.EvCompleted,
		Data: map[string]any{"head": head, "digest": digest, "record": record}}
}

func TestRequiredStatusRepairRecordSource(t *testing.T) {
	match := verifyRow("call-1", "h1", "d1", `{"schema":"r1"}`)
	foreign := []engine.Event{
		verifyRow("call-2", "h1", "d1", `{"foreign":"call"}`),
		verifyRow("call-1", "h2", "d1", `{"foreign":"head"}`),
		verifyRow("call-1", "h1", "d2", `{"foreign":"digest"}`),
		{Seq: 2, Kind: engine.KindCall, Op: OpGate, CallID: "call-1", Event: engine.EvCompleted,
			Data: map[string]any{"head": "h1", "digest": "d1", "record": `{"foreign":"op"}`}},
	}
	if got := repairVerification(rows{match}, "call-1", "h1", "d1"); string(got) != `{"schema":"r1"}` {
		t.Fatalf("the matching row's record = %q", got)
	}
	if got := repairVerification(append(rows{match}, foreign...), "call-1", "h1", "d1"); string(got) != `{"schema":"r1"}` {
		t.Error("a foreign row shadowed the exact call/head/digest match")
	}
	for i, row := range foreign {
		if got := repairVerification(rows{row}, "call-1", "h1", "d1"); len(got) != 0 {
			t.Errorf("foreign row %d answered %q", i, got)
		}
	}
	noRecord := engine.Event{Seq: 3, Kind: engine.KindCall, Op: OpVerify, CallID: "call-1", Event: engine.EvCompleted,
		Data: map[string]any{"head": "h1", "digest": "d1"}}
	if got := repairVerification(rows{noRecord}, "call-1", "h1", "d1"); len(got) != 0 {
		t.Errorf("a matching row without a record answered %q", got)
	}
}
