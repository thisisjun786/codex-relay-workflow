package scenario

import (
	"slices"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/flow/engine"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/reviewadapter"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/acceptance/premerge"
)

// The round cap of the unit check and what it carries (CRW-1465, fix round x1436): what is left after the last round trip is carried
// before any tier step, only a check that rules needs_changes is tier evidence (SP-TIER-1 rule (a)), the carried findings reach the
// premerge record of the acceptance, and a tier step taken before any commit leaves the tier's work in a recorded commit.

// carriedResult is the value of a run with the carried ids of each lane.
type carriedResult struct {
	State string `json:"state"`
	Lanes []struct {
		Node    string   `json:"node"`
		State   string   `json:"state"`
		Reason  string   `json:"reason"`
		Carried []string `json:"carried"`
	} `json:"lanes"`
}

func runCarried(t *testing.T, l *Lab) (carriedResult, string, string, []string) {
	t.Helper()
	got := Value[carriedResult](t, l.Run())
	for _, ln := range got.Lanes {
		if ln.Node == "A" {
			return got, ln.State, ln.Reason, ln.Carried
		}
	}
	t.Fatalf("no lane A in %+v", got)
	return got, "", "", nil
}

// premergeDefects are the ids of the defects of the premerge record the relay was asked to accept for the node (its last acceptance).
func premergeDefects(t *testing.T, l *Lab, node string) []string {
	t.Helper()
	var ids []string
	found := false
	for _, a := range l.Relay.Accepts {
		if a.Node != node {
			continue
		}
		rec, err := premerge.Decode(a.Premerge)
		if err != nil {
			t.Fatalf("the premerge record of %s: %v", node, err)
		}
		ids, found = nil, true
		for _, d := range rec.Defects {
			ids = append(ids, d.ID)
		}
	}
	if !found {
		t.Fatalf("no acceptance of %s was asked", node)
	}
	return ids
}

func fixCalls(l *Lab) int {
	n := 0
	for _, lb := range l.Labels() {
		if strings.HasPrefix(lb, "fix:A:") {
			n++
		}
	}
	return n
}

// TestUnitCheckCarriesARepeatedFindingAfterTheRoundCapBeforeTheTierStep (x1436 item 1): a finding the third check repeats from the
// second (f1, f2, f2) is left after the two round trips, so it is carried before the tier evidence it also makes is read: a blocking
// one escalates unit_check_carried with it, a non-blocking one lands carried, and no executor-t1 starts in either case.
func TestUnitCheckCarriesARepeatedFindingAfterTheRoundCapBeforeTheTierStep(t *testing.T) {
	f1 := Finding("P2", "the first thing is wrong")
	t.Run("blocking", func(t *testing.T) {
		l := NewLab(t, oneNode()...)
		f2 := Finding("P2", "the second thing is wrong")
		l.UnitChecks("A", NeedsChanges(f1), NeedsChanges(f2), NeedsChanges(f2), UnitCheck{})
		got, state, reason, carried := runCarried(t, l)
		if got.State != "incomplete" || state != "escalated" || reason != "unit_check_carried" {
			t.Fatalf("%+v; calls %v", got, l.Labels())
		}
		if id := reviewadapter.FindingID(f2.File, f2.Text); !slices.Equal(carried, []string{id}) {
			t.Errorf("carried %v, want [%s]", carried, id)
		}
		if want := []string{"architect", "design-audit:r1", "build:A:executor", "fix:A:executor:r1", "fix:A:executor:r2"}; !slices.Equal(l.Labels(), want) {
			t.Errorf("calls %v, want %v", l.Labels(), want)
		}
		if n := len(l.Checker.Calls("A")); n != 3 {
			t.Errorf("%d unit checks, want 3", n)
		}
		esc := l.FlowRows(engine.EvEscalation)
		if len(esc) != 1 || str(esc[0].Data, "ask") != "land" || !strings.Contains(str(esc[0].Data, "detail"), f2.Text) {
			t.Errorf("escalations %+v", esc)
		}
		if len(l.Relay.Landed) != 0 {
			t.Errorf("landed %v", l.Relay.Landed)
		}
		noTierStep(t, l)
	})
	t.Run("non-blocking", func(t *testing.T) {
		l := NewLab(t, oneNode()...)
		f2 := Finding("P3", "a name could be clearer")
		l.UnitChecks("A", NeedsChanges(f1), UnitCheck{Findings: []reviewadapter.Finding{f2}}, UnitCheck{Findings: []reviewadapter.Finding{f2}}, UnitCheck{})
		got, state, _, carried := runCarried(t, l)
		if got.State != "closed" || state != "landed" {
			t.Fatalf("%+v; calls %v", got, l.Labels())
		}
		id := reviewadapter.FindingID(f2.File, f2.Text)
		if !slices.Equal(carried, []string{id}) {
			t.Errorf("carried %v, want [%s]", carried, id)
		}
		if want := []string{"architect", "design-audit:r1", "build:A:executor", "fix:A:executor:r1", "fix:A:executor:r2", "reflection:M1"}; !slices.Equal(l.Labels(), want) {
			t.Errorf("calls %v, want %v", l.Labels(), want)
		}
		if n := len(l.Checker.Calls("A")); n != 3 {
			t.Errorf("%d unit checks, want 3", n)
		}
		if ids := premergeDefects(t, l, "A"); !slices.Contains(ids, id) {
			t.Errorf("the premerge defects %v do not name the carried finding %s", ids, id)
		}
		noTierStep(t, l)
	})
}

// TestUnitCheckPersistentNonBlockingFindingIsCarriedAndLands (x1436 item 8, the DI probe TestDIFVPersistentNonBlockingFinding): a
// verified check's finding blocks nothing, so it is no tier evidence however often it repeats (SP-TIER-1 rule (a) counts only checks that
// rule needs_changes); it goes through the two round trips, and what is left after them is carried and the node lands with it.
func TestUnitCheckPersistentNonBlockingFindingIsCarriedAndLands(t *testing.T) {
	l := NewLab(t, oneNode()...)
	minor := Finding("P3", "a name could be clearer")
	l.OnUnitCheck(func(CheckInfo) (UnitCheck, bool) { return UnitCheck{Findings: []reviewadapter.Finding{minor}}, true })
	got, state, reason, carried := runCarried(t, l)
	if got.State != "closed" || state != "landed" {
		t.Fatalf("lane A: %s %s; escalations %v; calls %v", state, reason, escalationReasons(l), l.Labels())
	}
	id := reviewadapter.FindingID(minor.File, minor.Text)
	if !slices.Equal(carried, []string{id}) {
		t.Errorf("carried %v, want [%s]", carried, id)
	}
	if n, f := len(l.Checker.Calls("A")), fixCalls(l); n != 3 || f != 2 {
		t.Errorf("%d unit checks and %d fixes, want 3 and 2", n, f)
	}
	if !slices.Equal(l.Relay.Landed, []string{"A"}) {
		t.Errorf("landed %v", l.Relay.Landed)
	}
	if ids := premergeDefects(t, l, "A"); !slices.Contains(ids, id) {
		t.Errorf("the premerge defects %v do not name the carried finding %s", ids, id)
	}
	noTierStep(t, l)
}

// TestUnitCheckCarriedFindingIsInThePremergeRecordAfterAVerificationFix (x1436 item 4, DI item 9: the probes
// TestDIFVCarriedFindingInThePremergeRecordAfterAVerificationFix and its control): a non-blocking finding carried past the last round
// trip is a defect of the premerge record of the acceptance even when the head then failed its verification and the check of the fix,
// which judges only the fix, found nothing; with no verification failure it is the defect of the check the acceptance rests on.
func TestUnitCheckCarriedFindingIsInThePremergeRecordAfterAVerificationFix(t *testing.T) {
	minor := Finding("P3", "a name could be clearer")
	id := reviewadapter.FindingID(minor.File, minor.Text)
	for _, tc := range []struct {
		name string
		fail []int
	}{{"after a verification fix", []int{1}}, {"control: no verification failure", nil}} {
		t.Run(tc.name, func(t *testing.T) {
			l := NewLab(t, oneNode()...)
			l.UnitChecks("A", NeedsChanges(Finding("P2", "the first thing is wrong")), NeedsChanges(Finding("P2", "a second thing is wrong")),
				UnitCheck{Findings: []reviewadapter.Finding{minor}})
			if tc.fail != nil {
				l.Verify.Fail = map[string][]int{"A": tc.fail}
			}
			got, state, _, carried := runCarried(t, l)
			if got.State != "closed" || state != "landed" || !slices.Equal(carried, []string{id}) {
				t.Fatalf("%+v; calls %v", got, l.Labels())
			}
			if len(l.Relay.Accepts) != 1 {
				t.Fatalf("%d acceptances", len(l.Relay.Accepts))
			}
			if ids := premergeDefects(t, l, "A"); !slices.Contains(ids, id) {
				t.Errorf("the premerge record of A's acceptance names defects %v, not the carried finding %s", ids, id)
			}
			want := 3
			if tc.fail != nil {
				want = 4
			}
			if n := len(l.Checker.Calls("A")); n != want {
				t.Errorf("%d unit checks, want %d", n, want)
			}
		})
	}
}
