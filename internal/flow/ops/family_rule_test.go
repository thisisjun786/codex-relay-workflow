package ops

import (
	"encoding/json"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/flow/engine"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/reviewadapter"
)

func TestDesignAuditAcceptsSameFamilyAndOnlyWarnsOnSameThread(t *testing.T) {
	for _, tc := range []struct {
		name, criticModel, criticThread string
		warn                            bool
	}{
		{"same model separate thread", "claude-opus", "critic-thread", false},
		{"same family separate thread", "claude-sonnet", "critic-thread", false},
		{"unrecorded model", "", "critic-thread", false},
		{"same thread", "gpt-x", "author-thread", true},
		{"legacy missing thread", "claude-opus", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := newWorld(t)
			design, _ := json.Marshal(designOf("A"))
			cand := Candidate{Design: design, Answers: json.RawMessage(`[]`)}
			arch := call("a", "completed", "claude-opus", nil)
			arch.Meta.ThreadID = "author-thread"
			critic := call("r", "completed", tc.criticModel, map[string]any{"verdict": "approved", "findings": []any{}, "summary": "ok"})
			critic.Meta.ThreadID = tc.criticThread
			var result map[string]any
			if err := json.Unmarshal(w.must("designAudit", "M", 1, cand, arch, critic), &result); err != nil {
				t.Fatal(err)
			}
			if result["state"] != AuditApproved || result["reason"] != "" {
				t.Fatalf("audit must accept a valid verdict: %+v", result)
			}
			warnings, _ := result["warnings"].([]any)
			_, row := rowOf(w.j.Events(), OpDesignAudit)
			if tc.warn {
				if len(warnings) != 1 || warnings[0] != "review_same_thread" || row.Data["warnings"] == nil {
					t.Fatalf("same-thread review must warn and record without refusing: result=%v row=%v", result, row.Data)
				}
			} else if len(warnings) != 0 || row.Data["warnings"] != nil {
				t.Fatalf("no observed same thread: result=%v row=%v", result, row.Data)
			}
		})
	}
}

func TestDesignReuseAcceptsSameFamilyApproval(t *testing.T) {
	for _, model := range []string{"claude-y", ""} {
		t.Run("critic="+model, func(t *testing.T) {
			events := sourceRunEvents(t, true, designOf("A"))
			i, _ := rowOf(events, OpDesignAudit)
			events[i].Data["critic_model"] = model
			w := reuseWorld(t, DesignRun{Run: "run-source", Events: events})
			out := reuseDecision(t, w)
			if !out.Reused || out.SourceRun != "run-source" || out.CriticModel != model {
				t.Fatalf("standing approved design must be reusable regardless of model family: %+v", out)
			}
			w.must("designLedger", "M", map[string]any{"reuse": true})
			led, ok := currentLedger(rows(w.j.Events()), "M")
			if !ok || !reusedLedgerApproved(rows(w.j.Events()), "M", led) {
				t.Fatal("the imported approval lost its recorded reuse provenance")
			}
		})
	}
}

func TestCrosscheckDoesNotExcludeAuthorFamilies(t *testing.T) {
	for _, authors := range []map[string]string{
		{"architect": "gpt-6-astra", "executor": "grok-4.7"},
		{"architect": "", "executor": "claude-fake-haiku"},
		{},
	} {
		w := newCCWorld(t)
		w.authors = authors
		w.build()
		w.j.Append(engine.Event{RunID: "run-1", Kind: engine.KindCall, CallID: "unknown-author", Op: "agent", Event: engine.EvPrepared, Data: map[string]any{"role": "executor", "node": "n1", "model": ""}})
		out, _ := w.crosscheck(ccHead, nil)
		calls := w.checker.requests()
		if out.State != CrosscheckStateVerdict || len(calls) != 2 {
			t.Fatalf("author model families must not stop crosscheck: %+v, calls=%d", out, len(calls))
		}
		for _, request := range calls {
			if request.Request.Profile.Name != "astra" {
				t.Errorf("profile order changed with author models: %s", request.Request.Profile.Name)
			}
		}
	}
}

func TestCrosscheckStillNeedsAnEngineProfile(t *testing.T) {
	w := newCCWorld(t)
	w.lane.d.Crosscheck.Profiles = func(string) ([]reviewadapter.Profile, error) { return nil, nil }
	out, _ := w.crosscheck(ccHead, nil)
	if out.State != CrosscheckStateRefused || out.Reason != ReasonCrosscheckNoEngine || len(w.checker.requests()) != 0 {
		t.Fatalf("no engine is configured: %+v", out)
	}
}
