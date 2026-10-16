package ops

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestMilestoneCloseRefusesWithoutAVerifiedCrosscheck (CRW-1473): crw.milestoneClose closes a milestone only over a verified review of
// the gate's head. A crosscheck V must say verified and be of the milestone and of the gate's head; with no crosscheck, a final review
// must be a verified verdict (a journal of the close before the crosscheck); evidence with neither is refused. A refusal is
// close_unverified, and no close is recorded.
func TestMilestoneCloseRefusesWithoutAVerifiedCrosscheck(t *testing.T) {
	gateOf := func(head string) map[string]any {
		return map[string]any{"milestone": "M1", "head": head, "state": "pass"}
	}
	v := func(verdict, ms, head string) json.RawMessage {
		return json.RawMessage(`{"verdict":"` + verdict + `","guide_rev":"crw-crosscheck/1","milestone":"` + ms + `","head":"` + head + `","findings":[]}`)
	}
	finalOf := func(verdict string) map[string]any {
		return map[string]any{"state": "verdict", "verdict": map[string]any{"verdict": verdict, "criteria": []any{}, "defects": []any{}}}
	}
	other := strings.Repeat("9", 40)
	for _, c := range []struct {
		name     string
		evidence func(tip string) map[string]any
		refused  bool
	}{
		{"a needs_changes crosscheck", func(tip string) map[string]any {
			return map[string]any{"gate": gateOf(tip), "crosscheck": v("needs_changes", "M1", tip)}
		}, true},
		{"a verified crosscheck of another head", func(tip string) map[string]any {
			return map[string]any{"gate": gateOf(tip), "crosscheck": v("verified", "M1", other)}
		}, true},
		{"a verified crosscheck of another milestone", func(tip string) map[string]any {
			return map[string]any{"gate": gateOf(tip), "crosscheck": v("verified", "M2", tip)}
		}, true},
		{"a needs_changes final and no crosscheck", func(tip string) map[string]any {
			return map[string]any{"gate": gateOf(tip), "final": finalOf("needs_changes")}
		}, true},
		{"neither a crosscheck nor a final", func(tip string) map[string]any { return map[string]any{"gate": gateOf(tip)} }, true},
		{"a null crosscheck and a null final", func(tip string) map[string]any {
			return map[string]any{"gate": gateOf(tip), "crosscheck": nil, "final": nil}
		}, true},
		{"a verified crosscheck at the gate head", func(tip string) map[string]any {
			return map[string]any{"gate": gateOf(tip), "crosscheck": v("verified", "M1", tip)}
		}, false},
		{"a verified final and no crosscheck", func(tip string) map[string]any {
			return map[string]any{"gate": gateOf(tip), "final": finalOf("verified")}
		}, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			w := newWorld(t)
			before := len(w.j.Events())
			raw, err := w.op(NameMilestoneClose, "M1", c.evidence(w.relay.tip))
			if !c.refused {
				if err != nil {
					t.Fatalf("the close was refused: %v", err)
				}
				var out CloseResultOutcome
				if err := json.Unmarshal(raw, &out); err != nil {
					t.Fatal(err)
				}
				if out.Tip != w.relay.tip || out.Moved || len(w.j.Events()) != before+1 {
					t.Errorf("the close %+v was not recorded", out)
				}
				return
			}
			if err == nil || ReasonOf(err.Error()) != "close_unverified" {
				t.Fatalf("the close was not refused close_unverified: %v (%s)", err, raw)
			}
			if len(w.j.Events()) != before {
				t.Errorf("a refused close left a row")
			}
		})
	}
}
