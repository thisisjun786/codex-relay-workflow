package ops

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func TestIntegratedDesign1658DuplicateDecisionRefs(t *testing.T) {
	for _, changed := range []bool{false, true} {
		name := "unchanged"
		if changed {
			name = "changed D1"
		}
		t.Run(name, func(t *testing.T) {
			w, _, source := integratedDesignWorld(t)
			d := designOf("A")
			d.Issues[0].DecisionRefs = []string{"D1", "D1"}
			base := sourceRunEvents(t, true, d)
			_, led := rowOf(base, OpDesignLedger)
			led.Data["base_tip"] = "base"
			_, release := rowOf(source, OpRelease)
			release.Data["design_digest"] = dataStr(led, "digest")
			source = append(base, source[len(base):]...)
			w.lane.d.DesignHistory = func(context.Context, string) ([]DesignRun, error) {
				return []DesignRun{{Run: "source", Events: source}}, nil
			}
			w.must(NameDesignReuse, "M")
			if changed {
				d.Decisions[0].Change += "!"
			}
			raw := result(d)
			cand := Candidate{Design: raw, Answers: json.RawMessage(`[]`)}
			w.must(NameDesignAudit, "M", 1, cand, call("arch", "completed", "model", nil), call("critic", "completed", "other", map[string]any{"verdict": "approved", "findings": []any{}, "summary": "ok"}))
			_, err := w.op(NameDesignLedger, "M", LedgerEntry{Design: raw, Answers: cand.Answers})
			if changed {
				if err == nil || !strings.Contains(err.Error(), "must keep decision D1") {
					t.Fatalf("changed D1 was not refused: %v", err)
				}
			} else if err != nil {
				t.Fatalf("unchanged DESIGN with duplicate references refused: %v", err)
			}
			fixed, err := integratedDesigns(rows(w.j.Events()), "M")
			if err != nil || len(fixed) != 1 || len(fixed[0].Decisions) != 1 || fixed[0].Decisions[0].ID != "D1" {
				t.Fatalf("source decision collected more than once: %+v, %v", fixed, err)
			}
		})
	}
}
