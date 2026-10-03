package daemon

import (
	"fmt"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/delivery"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// admit is the statement that admits a turn to generation 1 on the given evidence.
func admit(turn, evidence string) string {
	return fmt.Sprintf("INSERT INTO generation_turns(relationship_id,execution_generation,turn_id,evidence,actor,detail,admitted_at) VALUES('r',1,'%s','%s','child','admitted','2023-11-14T22:13:20Z')", turn, evidence)
}

// revision is the setup for a newer revision of the receipt on "continuation": a final child event on
// a newly admitted turn that declares it supersedes the first revision, so the first is replaced and
// this one is the head. It is owed a delivery only when owed is set.
func revision(turn string, owed bool) []string {
	statements := []string{
		admit(turn, "explicit_admission_bound:anchor"),
		"INSERT INTO events(event_id,relationship_id,execution_generation,revision_hash,outcome,producer,turn_thread_id,turn_id,turn_status,receipt,stage,staged_at,finalized_at,finalizing_status,first_seen_at,last_seen_at) VALUES('claim-" + turn + "','r',1,'rev2','ready_for_review','child','child','" + turn + "','completed','{}','final','2023-11-14T22:13:20Z','2023-11-14T22:13:20Z','completed','2023-11-14T22:13:20Z','2023-11-14T22:13:20Z')",
		"INSERT INTO revision_lineage(relationship_id,execution_generation,event_id,revision_hash,supersedes_hash,declared_by,recorded_at) VALUES('r',1,'claim-" + turn + "','rev2','rev','child_declared','2023-11-14T22:13:20Z')",
	}
	if owed {
		statements = append(statements, "INSERT INTO deliveries(event_id,relationship_id,kind,recipient_task_id,recipient_thread_id,state,attempt_count,created_at,updated_at) VALUES('claim-"+turn+"','r','completion','parent','parent','queued',0,'2023-11-14T22:13:20Z','2023-11-14T22:13:20Z')")
	}
	return statements
}

// A later receipt only counts for a turn the store would admit: the anchor, or a turn admitted against
// the anchor before the turn that holds the receipt, and the receipt must sit on a turn admitted the
// same way. It also needs a delivery still owed to the parent (not one a newer revision replaced), and
// the relationship must still be active on the generation it stands on now. Anything else matches nothing, so the observation path decides as it
// did before, and still refuses a turn the relay never admitted as unassigned.
func TestLaterReceiptIsFoundOnlyForTurnsTheStoreAdmits(t *testing.T) {
	t.Parallel()
	const bound = "explicit_admission_bound:anchor"
	owed := lateClaim{"continuation", "child", "final", owedQueued}
	for _, c := range []struct {
		name     string
		setup    []string // run before the receipt is stored
		claim    lateClaim
		observed string
		thread   string // the thread of the observed turn, the child's when empty
		want     string // the turn the receipt is found on, empty for none
	}{
		{"the anchor, a later admitted turn reported", nil, owed, "anchor", "", "continuation"},
		{"an admitted turn, a later admitted turn reported", nil, owed, "business", "", "continuation"},
		{"the last admitted turn", nil, owed, "continuation", "", ""},
		{"a turn nobody admitted", nil, owed, "stranger", "", ""},
		{"a turn admitted on other evidence, a turn admitted after it reported", []string{admit("odd", "not_the_anchor"), admit("after", bound)}, lateClaim{"after", "child", "final", owedQueued}, "odd", "", ""},
		{"a receipt on a later turn admitted on other evidence", []string{admit("odd", "not_the_anchor")}, lateClaim{"odd", "child", "final", owedQueued}, "business", "", ""},
		{"the anchor's own receipt, the anchor admitted as a turn too", []string{admit("anchor", bound)}, lateClaim{"anchor", "child", "final", owedQueued}, "anchor", "", ""},
		{"a receipt owed no delivery", nil, lateClaim{"continuation", "child", "final", ""}, "business", "", ""},
		{"a receipt whose delivery was superseded", nil, lateClaim{"continuation", "child", "final", "superseded"}, "business", "", ""},
		{"a receipt a newer revision replaced, the newer one owed no delivery", revision("second", false), owed, "business", "", ""},
		{"a receipt already sent, then replaced by a revision owed no delivery", revision("second", false), lateClaim{"continuation", "child", "final", "dispatched"}, "business", "", "continuation"},
		{"a receipt stored in the recipient's inbox, then replaced by a revision owed no delivery", revision("second", false), lateClaim{"continuation", "child", "final", "inbox_only"}, "business", "", "continuation"},
		{"a receipt a newer revision replaced, the newer one owed a delivery", revision("second", true), owed, "business", "", "second"},
		{"a thread that is not the child's", nil, owed, "business", "elsewhere", ""},
		{"the relationship was superseded", []string{"UPDATE relationships SET superseded_by='r2' WHERE relationship_id='r'"}, owed, "business", "", ""},
		{"the relationship is no longer active", []string{"UPDATE relationships SET status='paused' WHERE relationship_id='r'"}, owed, "business", "", ""},
		{"the generation moved on", []string{
			"INSERT INTO generations(relationship_id,execution_generation,dispatch_request_id,anchor_state,dispatch_turn_id,reason,opened_at,bound_at) VALUES('r',2,'dispatch-r-2','bound','anchor2','revision','2023-11-14T22:13:20Z','2023-11-14T22:13:20Z')",
			"UPDATE relationships SET execution_generation=2 WHERE relationship_id='r'",
		}, owed, "business", "", ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			ctx, s := lateStore(t)
			for _, statement := range c.setup {
				if _, err := s.DB.Exec(statement); err != nil {
					t.Fatal(err)
				}
			}
			c.claim.insert(t, s)
			d := New(s, &observationHost{}, &delivery.FakeClock{T: 1700000000}, nil)
			thread := c.thread
			if thread == "" {
				thread = "child"
			}
			later, event, err := d.laterReceipt(ctx, "r", store.TurnReference{ThreadID: thread, TurnID: c.observed, Status: "interrupted"})
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
	t.Parallel()
	ctx, s := lateStore(t)
	lateClaim{"continuation", "child", "final", owedQueued}.insert(t, s)
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
