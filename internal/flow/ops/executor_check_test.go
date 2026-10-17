package ops

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/executor"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/dagsched"
)

type resultCheckController struct {
	bound        dagsched.ExecutorCheckContext
	reads, gates int
	changed      bool
}

func (s *resultCheckController) ExecutorCheckContext(context.Context, string, string) (dagsched.ExecutorCheckContext, error) {
	s.reads++
	if s.changed && s.reads > 1 {
		return s.bound, errors.New("assignment is stale")
	}
	return s.bound, nil
}
func (s *resultCheckController) CheckExecutorGates(context.Context, executor.Identity, string, *executor.CollectedCandidate, []byte, []byte) ([]executor.RequiredGate, error) {
	s.gates++
	return []executor.RequiredGate{{Gate: "review_or_unit_check", Reason: "no current genuine verdict"}}, nil
}
func TestExecutorResultCheckRechecksAuthorityAndKeepsMissingGates(t *testing.T) {
	task := execTask(executor.Devin)
	task.WorkKind = "design"
	task.Checks = nil
	task.NotApplicable = "approved design-only review"
	a, err := executor.Freeze(task)
	if err != nil {
		t.Fatal(err)
	}
	unknown := executor.Observation{UnknownReason: "not observed"}
	r, err := executor.NewResult(a, "completed", executor.Payload{Narrative: "reviewer verified"}, executor.Provenance{Provider: executor.Devin, NativeSession: unknown, Model: unknown, Effort: unknown, CPUMethod: unknown})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := executor.EncodeResult(r)
	if err != nil {
		t.Fatal(err)
	}
	spool := filepath.Join(t.TempDir(), "spool")
	if err = os.Mkdir(spool, 0700); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(spool, "result.json"), raw, 0600); err != nil {
		t.Fatal(err)
	}
	s := &resultCheckController{bound: dagsched.ExecutorCheckContext{Admission: a, Repository: executor.RegisteredRepository{Forge: a.Task.Repository.Forge, ID: a.Task.Repository.ID, Source: "https://example.invalid/registered"}}}
	in := ExecutorResultCheck{Execution: a.Task.Identity.ExecutionID, Actor: "parent", Spool: spool, ResultPath: "result.json"}
	got, err := CheckExecutorResult(context.Background(), s, in)
	if err != nil || !got.EnvelopeValid || got.CandidateAccepted || s.reads != 2 || s.gates != 1 {
		t.Fatalf("report=%+v controller=%+v err=%v", got, s, err)
	}
	found := false
	for _, g := range got.RequiredGates {
		if g.Gate == "review_or_unit_check" && g.Reason == "no current genuine verdict" {
			found = true
		}
	}
	if !found {
		t.Fatal("actual missing gate reason lost")
	}
	s.reads = 0
	s.gates = 0
	s.changed = true
	if _, err = CheckExecutorResult(context.Background(), s, in); err == nil || s.gates != 0 {
		t.Fatal("stale assignment reached gate judgments")
	}
}
