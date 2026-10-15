package workflows

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/flow/engine"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/reviewadapter"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/script"
)

// evidenceOut runs a script that imports from crw:lib/evidence, with body ending in a return, and returns the value it returns.
func evidenceOut(t *testing.T, name, body string) map[string]any {
	t.Helper()
	src := "import { evidence } from 'crw:lib/evidence'\nexport const meta = { name: '" + name + "' }\n" + body + "\n"
	prog, err := script.Load(script.Source{Name: name + ".js", Text: []byte(src)})
	if err != nil {
		t.Fatal(err)
	}
	res, err := engine.Run(context.Background(), prog, engine.Config{Backend: engine.NewFake(), Policy: engine.FakePolicy()})
	if err != nil || res.Status != engine.RunCompleted {
		t.Fatalf("%v %+v", err, res)
	}
	var out map[string]any
	if err := json.Unmarshal(res.Value, &out); err != nil {
		t.Fatalf("the script did not return an object: %v (%s)", err, res.Value)
	}
	return out
}

func strs(t *testing.T, v any) []string {
	t.Helper()
	list, ok := v.([]any)
	if !ok {
		t.Fatalf("%v is not a list", v)
	}
	out := make([]string, 0, len(list))
	for _, e := range list {
		s, ok := e.(string)
		if !ok {
			t.Fatalf("%v of %v is not a string", e, v)
		}
		out = append(out, s)
	}
	return out
}

// TestEvidenceTrackerCountsUnitCheckFindingsWithoutLineNumbers (CRW-1465, D-10): in the node lane the unit check takes the place
// of the reviewer. Rule (a) of SP-TIER-1 is two consecutive unit checks of the tier whose findings share a finding id, and the id
// is the adapter's (reviewadapter.FindingID: file and text, no line), so a finding that moved down the file between the two checks
// is the same finding. The tracker reads the id and never the line. Different texts in the same file are different findings; a
// check without findings ends the run. evidence() lists the checks as unit_check_calls (reviewer_calls is empty for them) and
// ids() names them; (d) counts the checks as reviewed rounds.
func TestEvidenceTrackerCountsUnitCheckFindingsWithoutLineNumbers(t *testing.T) {
	nilDeref := reviewadapter.FindingID("internal/a.go", "the handle is nil after Close")
	nilDerefAgain := reviewadapter.FindingID("internal/a.go", "The handle is  nil after Close")
	leak := reviewadapter.FindingID("internal/a.go", "the file is never closed")
	if nilDeref != nilDerefAgain || nilDeref == leak {
		t.Fatalf("FindingID %s %s %s", nilDeref, nilDerefAgain, leak)
	}
	finding := func(id string, line int) string {
		return fmt.Sprintf("{ id: %q, severity: 'P1', file: 'internal/a.go', line: %d, text: 'x' }", id, line)
	}
	body := fmt.Sprintf(`const out = {}
{ const ev = evidence('A')
  ev.unitCheck('executor', 'uc1', 'needs_changes', [%[1]s])
  out.oneCheck = ev.due('executor')
  ev.unitCheck('executor', 'uc2', 'needs_changes', [%[2]s])
  out.movedLine = ev.due('executor')
  out.evidence = ev.evidence('executor')
  out.ids = ev.ids('executor') }
{ const ev = evidence('A')
  ev.unitCheck('executor', 'uc1', 'needs_changes', [%[5]q])
  ev.unitCheck('executor', 'uc2', 'needs_changes', [%[6]q])
  out.sameIdAsText = ev.due('executor') }
{ const ev = evidence('A')
  ev.unitCheck('executor', 'uc1', 'needs_changes', [%[1]s])
  ev.unitCheck('executor', 'uc2', 'needs_changes', [%[3]s])
  out.differentText = ev.due('executor') }
{ const ev = evidence('A')
  ev.unitCheck('executor', 'uc1', 'needs_changes', [%[1]s])
  ev.unitCheck('executor', 'uc2', 'verified', [])
  ev.unitCheck('executor', 'uc3', 'needs_changes', [%[2]s])
  out.notConsecutive = ev.due('executor') }
{ const ev = evidence('A')
  ev.unitCheck('executor', 'uc1', 'verified', [%[4]s])
  ev.unitCheck('executor', 'uc2', 'verified', [%[2]s])
  out.minorRepeated = ev.due('executor')
  out.otherTier = ev.due('executor-t1') }
{ const ev = evidence('A')
  for (const i of [1, 2, 3, 4]) ev.unitCheck('executor', 'uc' + i, 'needs_changes', ['f' + i])
  out.fourChecks = ev.due('executor') }
return out`, finding(nilDeref, 10), finding(nilDerefAgain, 42), finding(leak, 42), finding(nilDeref, 3), nilDeref, nilDerefAgain)
	got := evidenceOut(t, "evidence-unit-check", body)
	want := map[string]any{"oneCheck": nil, "movedLine": "a", "sameIdAsText": "a", "differentText": nil, "notConsecutive": nil,
		"minorRepeated": "a", "otherTier": nil, "fourChecks": "d"}
	for k, w := range want {
		if got[k] != w {
			t.Errorf("%s = %v, want %v", k, got[k], w)
		}
	}
	ev, ok := got["evidence"].(map[string]any)
	if !ok {
		t.Fatalf("evidence %v", got["evidence"])
	}
	if ev["letter"] != "a" || ev["rounds"] != float64(2) {
		t.Errorf("evidence %v", ev)
	}
	if calls := strs(t, ev["unit_check_calls"]); !slices.Equal(calls, []string{"uc1", "uc2"}) {
		t.Errorf("unit_check_calls %v, want [uc1 uc2]", calls)
	}
	if calls := strs(t, ev["reviewer_calls"]); len(calls) != 0 {
		t.Errorf("reviewer_calls %v, want none for unit checks", calls)
	}
	if ids := strs(t, got["ids"]); !slices.Contains(ids, "uc1") || !slices.Contains(ids, "uc2") {
		t.Errorf("ids %v do not name the unit checks", ids)
	}
}
