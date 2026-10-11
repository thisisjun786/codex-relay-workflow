package fsm

import (
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/state"
)

// CRW-146: unbind is a CRW addition (no oracle counterpart): it releases the goalplan slug a session is bound to, and only from rest.

func TestUnbindIsAChatVerb(t *testing.T) {
	cmd := ParseOrchestrateCommand("orchestrate unbind")
	if cmd == nil || cmd.Verb != VerbUnbind {
		t.Fatalf("parsed %+v, want the unbind verb", cmd)
	}
	if cmd := ParseOrchestrateCommand("/orchestrate UNBIND"); cmd == nil || cmd.Verb != VerbUnbind {
		t.Fatalf("the verb folds case and takes the prefix like the other verbs: %+v", cmd)
	}
}

func TestUnbindClearsOnlyTheSlugAndItsWorkPhasePointer(t *testing.T) {
	s := state.DefaultState("s1", "the-slug")
	pointer, phase := "wp-1", state.PhaseB
	s.StopBlockWorkPhaseID, s.StopBlockPhase, s.StopBlockCount = &pointer, &phase, 2
	s.LoopArmSeen, s.IdleEditNudges = true, 0
	s.StopMetricCursor, s.StopBlockTotal = 7, 3
	res := apply(s, VerbUnbind, nil)
	if !res.OK || res.Control != ControlUnbind || res.State == nil || res.Noop {
		t.Fatalf("result %+v", res)
	}
	want := s
	want.Slug, want.StopBlockWorkPhaseID = "", nil
	got := *res.State
	got.UpdatedAt, want.UpdatedAt = "", ""
	if gotJSON, wantJSON := mustEncode(t, got), mustEncode(t, want); gotJSON != wantJSON {
		t.Fatalf("state\n got %s\nwant %s", gotJSON, wantJSON)
	}
	row := res.Ledger
	if row == nil || row.SessionID != "s1" || row.From == nil || *row.From != state.PhaseIdle || row.To != state.PhaseIdle || row.Reason != "unbound" ||
		row.Evidence == nil || !strings.Contains(*row.Evidence, "the-slug") {
		t.Fatalf("ledger row %+v", row)
	}
}

func TestUnbindWithoutASlugIsANoop(t *testing.T) {
	res := apply(state.DefaultState("s1", ""), VerbUnbind, nil)
	if !res.OK || !res.Noop || res.Control != ControlUnbind || res.State != nil || res.Ledger != nil {
		t.Fatalf("result %+v", res)
	}
}

func TestUnbindIsRefusedWhileACycleIsInFlight(t *testing.T) {
	for _, phase := range []state.Phase{state.PhaseI, state.PhaseP, state.PhaseA, state.PhaseB, state.PhaseC, state.PhaseD} {
		s := state.DefaultState("s1", "the-slug")
		s.Phase, s.OrchestrationActive = phase, true
		res := apply(s, VerbUnbind, nil)
		if res.OK || res.State != nil || res.Ledger != nil {
			t.Fatalf("phase %s: result %+v", phase, res)
		}
		for _, want := range []string{"phase " + string(phase), "reset", "--session s1"} {
			if !strings.Contains(res.Reason, want) {
				t.Errorf("phase %s: reason %q lacks %q", phase, res.Reason, want)
			}
		}
	}
}

func TestUnbindIsRefusedWhileACloseIsPending(t *testing.T) {
	epoch := "c-1"
	s := state.DefaultState("s1", "the-slug")
	s.CheckEpoch = &epoch
	if res := apply(s, VerbUnbind, nil); res.OK || !strings.Contains(res.Reason, "reset") {
		t.Fatalf("check epoch: %+v", res)
	}
	s.CheckEpoch = nil
	s.DcloseRecovery = &state.DcloseRecoveryMarker{SessionID: "s1", CheckEpoch: "c-1", ClosedWorkPhaseID: "wp-1"}
	if res := apply(s, VerbUnbind, nil); res.OK || !strings.Contains(res.Reason, "reset") {
		t.Fatalf("d-close marker: %+v", res)
	}
}

func mustEncode(t *testing.T, s state.State) string {
	t.Helper()
	b, err := state.Encode(s)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
