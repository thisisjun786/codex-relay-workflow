package ops

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/flow/engine"
)

type integratedDesignRelay struct {
	*stubRelay
	state IntegratedState
	err   error
}

func (r *integratedDesignRelay) Integrated(context.Context, string, PlanNode) (IntegratedState, error) {
	return r.state, r.err
}

func integratedDesignWorld(t *testing.T) (*world, *integratedDesignRelay, []engine.Event) {
	t.Helper()
	source := sourceRunEvents(t, true, designOf("A"))
	_, led := rowOf(source, OpDesignLedger)
	led.Data["base_tip"] = "base"
	appendRow := func(op, call string, data map[string]any) {
		source = append(source, engine.Event{Seq: len(source) + 1, Kind: engine.KindCall, Event: engine.EvCompleted, Op: op, CallID: call, Data: data})
	}
	appendRow(OpRelease, "release", map[string]any{"plan": "plan-1", "milestone": "M", "node": "A", "released": true, "slice_digest": "slice-a", "criteria_digest": "criteria-a", "revision": "r0", "design_digest": dataStr(led, "digest")})
	appendRow(OpAccept, "accept", map[string]any{"plan": "plan-1", "node": "A", "head": "head", "acceptance_id": "acceptance", "basis": "unit_check", "reviewer_call": "review"})
	appendRow(OpIntegrate, "integrate", map[string]any{"plan": "plan-1", "node": "A", "head": "head", "acceptance_id": "acceptance", "integration_ref": "dev", "batch_commit": "batch"})
	w := reuseWorld(t, DesignRun{Run: "source", Events: source})
	w.relay.plan.Title = "changed milestone"
	w.relay.plan.Nodes[0].CriteriaDigest = "criteria-a"
	w.must(NameMilestone, "plan-1", "M")
	r := &integratedDesignRelay{stubRelay: w.relay, state: IntegratedState{Integrated: true, Head: "head", AcceptanceID: "acceptance", IntegrationRef: "dev", Tip: "tip"}}
	w.lane.d.Relay = r
	w.lane.d.Git = func(_ context.Context, _ string, _ []string, argv ...string) (string, error) {
		if argv[0] != "rev-parse" && strings.Join(argv[:2], " ") != "merge-base --is-ancestor" {
			t.Fatalf("unexpected git: %v", argv)
		}
		return "", nil
	}
	return w, r, source
}

func TestIntegratedDesignUsesBuildProofAndKeepsBuildRefusals(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*world, *integratedDesignRelay, []engine.Event)
		proven bool
	}{
		{"proven", func(*world, *integratedDesignRelay, []engine.Event) {}, true},
		{"missing reader", func(w *world, _ *integratedDesignRelay, _ []engine.Event) { w.lane.d.Relay = w.relay }, false},
		{"reader error", func(_ *world, r *integratedDesignRelay, _ []engine.Event) { r.err = errors.New("reader unavailable") }, false},
		{"no history", func(w *world, _ *integratedDesignRelay, _ []engine.Event) { w.lane.d.DesignHistory = nil }, false},
		{"history read error", func(w *world, _ *integratedDesignRelay, _ []engine.Event) {
			w.lane.d.DesignHistory = func(context.Context, string) ([]DesignRun, error) { return nil, errors.New("history unavailable") }
		}, false},
		{"missing audit", func(_ *world, _ *integratedDesignRelay, s []engine.Event) {
			_, e := rowOf(s, OpAccept)
			delete(e.Data, "reviewer_call")
		}, false},
		{"changed slice", func(_ *world, _ *integratedDesignRelay, s []engine.Event) {
			_, e := rowOf(s, OpRelease)
			e.Data["slice_digest"] = "other"
		}, false},
		{"missing batch", func(_ *world, _ *integratedDesignRelay, s []engine.Event) {
			_, e := rowOf(s, OpIntegrate)
			delete(e.Data, "batch_commit")
		}, false},
		{"incomplete relay", func(_ *world, r *integratedDesignRelay, _ []engine.Event) { r.state.Head = "" }, false},
		{"lost ancestry", func(w *world, _ *integratedDesignRelay, _ []engine.Event) {
			w.lane.d.Git = func(context.Context, string, []string, ...string) (string, error) {
				return "", errors.New("commit missing")
			}
		}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w, r, source := integratedDesignWorld(t)
			tc.mutate(w, r, source)
			w.must(NameDesignReuse, "M")
			fixed, err := integratedDesigns(rows(w.j.Events()), "M")
			if err != nil || len(fixed) != 1 || fixed[0].Reused != tc.proven {
				t.Fatalf("fixed=%+v err=%v", fixed, err)
			}
			if tc.proven && (fixed[0].SourceRun != "source" || fixed[0].Head != "head" || !reflect.DeepEqual(canonicalIssue(fixed[0].Issue), canonicalIssue(designOf("A").Issues[0]))) {
				t.Fatalf("source not preserved: %+v", fixed[0])
			}
			w.ledgered(true, designOf("A"))
			out, err := w.op(NameReuseIntegrated, "M", "A")
			if tc.proven {
				var reused ReuseIntegratedResult
				if err != nil || json.Unmarshal(out, &reused) != nil || !reused.Reused {
					t.Fatalf("build reuse: %s %v", out, err)
				}
			} else if tc.name != "missing reader" && err == nil {
				t.Fatalf("build accepted unproven landing: %s", out)
			}
		})
	}
}

func TestIntegratedDesignLedgerRejectsChangedIssueOrDecision(t *testing.T) {
	for _, what := range []string{"issue", "decision", "missing issue"} {
		t.Run(what, func(t *testing.T) {
			w, _, _ := integratedDesignWorld(t)
			w.must(NameDesignReuse, "M")
			d := designOf("A")
			switch what {
			case "issue":
				d.Issues[0].Approach += "!"
			case "decision":
				d.Decisions[0].Change += "!"
			case "missing issue":
				d.Issues = []DesignIssue{}
			}
			raw := result(d)
			cand := Candidate{Design: raw, Answers: json.RawMessage(`[]`)}
			w.must(NameDesignAudit, "M", 1, cand, call("arch", "completed", "model", nil), call("critic", "completed", "other", map[string]any{"verdict": "approved", "findings": []any{}, "summary": "ok"}))
			if _, err := w.op(NameDesignLedger, "M", LedgerEntry{Design: raw, Answers: cand.Answers}); err == nil || !strings.Contains(err.Error(), "integrated node A must keep") {
				t.Fatalf("ledger accepted %s: %v", what, err)
			}
		})
	}
}

func TestReuseIntegratedStillRefusesChangedDesign(t *testing.T) {
	for _, what := range []string{"issue", "decision"} {
		t.Run(what, func(t *testing.T) {
			w, _, _ := integratedDesignWorld(t)
			d := designOf("A")
			if what == "issue" {
				d.Issues[0].Approach += "!"
			} else {
				d.Decisions[0].Change += "!"
			}
			// An old run or another workflow can have its own audited, changed
			// ledger without the new design discovery. Build still refuses it.
			w.ledgered(true, d)
			_, err := w.op(NameReuseIntegrated, "M", "A")
			if err == nil || !strings.Contains(err.Error(), ReasonIntegrationUnproven) {
				t.Fatalf("changed %s no longer refused by build: %v", what, err)
			}
		})
	}
}

func TestDesignWithoutIntegrationsKeepsOriginalReuseRow(t *testing.T) {
	w := reuseWorld(t)
	w.lane.d.Relay = &integratedDesignRelay{stubRelay: w.relay}
	req := engine.OpRequest{RunID: "run-1", Journal: w.j, Args: json.RawMessage(`["M"]`)}
	want, err := (&designReuseOp{w.lane}).Run(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	got, err := w.lane.Ops()[NameDesignReuse].Run(context.Background(), req)
	if err != nil || !reflect.DeepEqual(want, got) {
		t.Fatalf("ordinary design reuse row changed: want=%+v got=%+v err=%v", want, got, err)
	}
}
