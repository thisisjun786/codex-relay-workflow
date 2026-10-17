package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/executor"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/ops"
)

func TestExecutorResultCheckPrintsValiditySeparatelyFromAcceptance(t *testing.T) {
	var out, errout bytes.Buffer
	code := runExecutorResultCheck(context.Background(), []string{"--execution", "e", "--actor", "parent", "--epoch", "1", "--spool", "/local/spool"}, &out, &errout, func(_ context.Context, state, socket string, epoch int64, in ops.ExecutorResultCheck) (executor.ResultCheckReport, error) {
		if in.Execution != "e" || in.Spool != "/local/spool" || in.ResultPath != "result.json" || epoch != 1 {
			t.Fatalf("input: %+v", in)
		}
		return executor.ResultCheckReport{EnvelopeValid: true, CandidateValid: true, CollectionStatus: "collected", RequiredGates: []executor.RequiredGate{{Gate: "review_or_unit_check", Reason: "no genuine flow-journal verdict"}}}, nil
	})
	var report executor.ResultCheckReport
	if err := json.Unmarshal(out.Bytes(), &report); err != nil {
		t.Fatal(err)
	}
	if code != 0 || !report.EnvelopeValid || report.CandidateAccepted || len(report.RequiredGates) != 1 {
		t.Fatalf("code=%d report=%+v stderr=%s", code, report, errout.String())
	}
}
func TestExecutorResultCheckRequiresControllerInputs(t *testing.T) {
	for _, args := range [][]string{{"result"}, {"result", "bogus"}, {"result", "check"}, {"result", "check", "--execution", "e", "--actor", "p", "--epoch", "1", "--spool", "remote/relative"}} {
		var out, errout bytes.Buffer
		if code := ExecutorRun(context.Background(), args, &out, &errout); code != 2 {
			t.Fatalf("%v: exit %d: %s", args, code, errout.String())
		}
	}
}
