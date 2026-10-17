package scenario

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/flow/engine"
)

func TestExplorer1611ScenarioMissingVerifiedAvoidsRetry(t *testing.T) {
	l := NewLab(t, twoNodes()...)
	const answer = "background without a verification claim"
	s := scriptBackground(l, "B", func(_ int, _ *Call) *fakehostPlan {
		p := Say(noVerified(answer))
		return &p
	})
	if got := mustRun(t, l); got.State != "closed" {
		t.Fatalf("state = %q, want closed", got.State)
	}
	if threads := s.Threads(); len(threads) != 1 {
		t.Fatalf("explorer threads = %v, want one", threads)
	}
	calls := backgroundCalls(l)
	if len(calls) != 2 {
		t.Fatalf("background calls = %v, want one per node", labelsOf(calls))
	}
	for _, call := range calls {
		if strings.HasSuffix(call.Label, ":retry") {
			t.Fatalf("unexpected retry = %s", call.Label)
		}
	}
	normalized := 0
	for _, row := range l.Events() {
		if row.Kind != engine.KindCall || row.Event != engine.EvCompleted || !strings.HasPrefix(row.Label, BackgroundLabel) {
			continue
		}
		if !strings.Contains(str(row.Data, "raw_text"), answer) {
			continue
		}
		normalized++
		if fmt.Sprint(row.Data["normalizations"]) != "[explorer_verified_missing_to_empty]" || row.Data["repair_used"] != false {
			t.Fatalf("normalization record = %v", row.Data)
		}
		var output map[string]any
		if err := json.Unmarshal([]byte(str(row.Data, "result")), &output); err != nil {
			t.Fatal(err)
		}
		verified, ok := output["verified"].([]any)
		if !ok || verified == nil || len(verified) != 0 || output["answer"] != answer {
			t.Fatalf("normalized findings = %v", output)
		}
	}
	if normalized != 1 {
		t.Fatalf("normalized completed rows = %d, want one", normalized)
	}
}
