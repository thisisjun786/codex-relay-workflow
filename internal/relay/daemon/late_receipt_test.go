package daemon

import (
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/delivery"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// A later receipt only counts for a turn the store would admit: the anchor, or a turn admitted against
// the anchor before the turn that holds the receipt, and the receipt must sit on a turn admitted the
// same way. Anything else matches nothing, so the observation path still refuses a turn the relay
// never admitted as unassigned.
func TestLaterReceiptIsFoundOnlyForTurnsTheStoreAdmits(t *testing.T) {
	const admit = "INSERT INTO generation_turns(relationship_id,execution_generation,turn_id,evidence,actor,detail,admitted_at) VALUES('r',1,?,?,'child','admitted','2023-11-14T22:13:20Z')"
	for _, c := range []struct {
		name     string
		admitted [][2]string // extra generation_turns rows: turn, evidence
		claim    string      // the turn holding a final child receipt
		observed string
		want     string // the turn the receipt is found on, empty for none
	}{
		{"the anchor, a later admitted turn reported", nil, "continuation", "anchor", "continuation"},
		{"an admitted turn, a later admitted turn reported", nil, "continuation", "business", "continuation"},
		{"the last admitted turn", nil, "continuation", "continuation", ""},
		{"a turn nobody admitted", nil, "continuation", "stranger", ""},
		{"a turn admitted on other evidence, a turn admitted after it reported", [][2]string{{"odd", "not_the_anchor"}, {"after", "explicit_admission_bound:anchor"}}, "after", "odd", ""},
		{"a receipt on a later turn admitted on other evidence", [][2]string{{"odd", "not_the_anchor"}}, "odd", "business", ""},
		{"the anchor's own receipt, the anchor admitted as a turn too", [][2]string{{"anchor", "explicit_admission_bound:anchor"}}, "anchor", "anchor", ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			ctx, s := lateStore(t)
			for _, row := range c.admitted {
				if _, err := s.DB.Exec(admit, row[0], row[1]); err != nil {
					t.Fatal(err)
				}
			}
			lateClaim{c.claim, "child", "final"}.insert(t, s)
			d := New(s, &observationHost{}, &delivery.FakeClock{T: 1700000000}, nil)
			rel, err := delivery.LoadRelationship(ctx, s, "r")
			if err != nil {
				t.Fatal(err)
			}
			later, event, err := d.laterReceipt(ctx, rel, store.TurnReference{ThreadID: "child", TurnID: c.observed, Status: "interrupted"})
			wantEvent := ""
			if c.want != "" {
				wantEvent = "claim-" + c.want
			}
			if err != nil || later != c.want || event != wantEvent {
				t.Fatalf("later turn %q event %q err %v, want %q %q", later, event, err, c.want, wantEvent)
			}
		})
	}
}

// A lookup that fails settles nothing, so the turn is read again on a later visit rather than lost.
func TestSettleKeepsATurnWhenTheLaterReceiptLookupFails(t *testing.T) {
	ctx, s := lateStore(t)
	lateClaim{"continuation", "child", "final"}.insert(t, s)
	d := New(s, &observationHost{}, &delivery.FakeClock{T: 1700000000}, nil)
	rel, err := delivery.LoadRelationship(ctx, s, "r")
	if err != nil {
		t.Fatal(err)
	}
	turn := store.TurnReference{ThreadID: "child", TurnID: "business", Status: "interrupted"}
	exec := func(statement string) {
		t.Helper()
		if _, err := s.DB.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	settled := func() (n int) {
		t.Helper()
		if err := s.DB.QueryRow("SELECT COUNT(*) FROM assignment_settlements WHERE turn_id='business'").Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	exec("ALTER TABLE generation_turns RENAME TO generation_turns_away")
	var failed Report
	d.settle(ctx, rel, turn, &failed)
	if failed.Observed != 0 || len(failed.Notes) != 1 || settled() != 0 {
		t.Fatalf("a failed lookup settled the turn: %+v settlements %d", failed, settled())
	}
	exec("ALTER TABLE generation_turns_away RENAME TO generation_turns")
	var retried Report
	d.settle(ctx, rel, turn, &retried)
	if retried.Observed != 1 || settled() != 1 {
		t.Fatalf("the retry did not settle the turn: %+v settlements %d", retried, settled())
	}
}
