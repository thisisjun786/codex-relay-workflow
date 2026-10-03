package supervisor

import (
	"context"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport/golden"
)

// checkSelection compares Channel.Selection's answer for o, as JSON decodes it, with the
// golden; the golden began as what Python's selection answered on the same store.
func checkSelection(t *testing.T, f *stageFixture, o Obligation, recipient string, now *float64) {
	t.Helper()
	got, err := f.c.Selection(context.Background(), o, recipient, now)
	if err != nil {
		t.Fatal(err)
	}
	golden.CheckJSON(t, goldenKey(t, "select"), asJSON(t, got), fixtureGolden(t, f.root)...)
}
func Test24_SR_5_ConfirmedVerdictDischarges(t *testing.T) {
	t.Parallel()
	f := fixture24(t)
	o := f.obligation(t)
	checkSelection(t, f, o, "", nil)
	if _, err := f.s.DB.Exec("INSERT INTO sync_targets (relationship_id,target,target_ref,recorded_at) VALUES ('rel-1','coordination_document','doc-1','t')"); err != nil {
		t.Fatal(err)
	}
	checkSelection(t, f, o, "", nil)
	for _, tc := range []struct{ id, state, ref, created string }{
		{"pending-one", "pending", "doc-1", "2023-11-14T22:13:20Z"},
		{"confirmed-two", "confirmed", "doc-1", "1999-01-01T00:00:00Z"},
		{"pending-three", "pending", "doc-1", "1980-01-01T00:00:00Z"},
	} {
		_, err := f.s.DB.Exec("INSERT INTO sync_outbox (sync_id,relationship_id,issue_key,target,target_ref,subject_kind,event_id,identity_digest,summary,state,created_at,updated_at) VALUES (?,?,?,?,?,'verdict',?,?,?,?,?,?)", tc.id, "rel-1", "REL-1", "coordination_document", tc.ref, "event-1", "digest", "summary", tc.state, tc.created, tc.created)
		if err != nil {
			t.Fatal(err)
		}
		checkSelection(t, f, o, "", nil)
	}
}
func Test24_SR_23_LatestRulingNotWallClock(t *testing.T) {
	t.Parallel()
	f := fixture24(t)
	o := f.obligation(t)
	if _, err := f.s.DB.Exec("INSERT INTO sync_targets (relationship_id,target,target_ref,recorded_at) VALUES ('rel-1','coordination_document','doc-1','t')"); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ id, state, created string }{{"old-confirmed", "confirmed", "2023-11-14T22:13:20Z"}, {"new-pending", "pending", "1999-01-01T00:00:00Z"}} {
		_, err := f.s.DB.Exec("INSERT INTO sync_outbox (sync_id,relationship_id,issue_key,target,target_ref,subject_kind,event_id,identity_digest,summary,state,created_at,updated_at) VALUES (?,?,?,?,?,'verdict',?,?,?,?,?,?)", tc.id, "rel-1", "REL-1", "coordination_document", "doc-1", "event-1", "digest", "summary", tc.state, tc.created, tc.created)
		if err != nil {
			t.Fatal(err)
		}
		checkSelection(t, f, o, "", nil)
	}
}
func Test24_SR_6_Contactability(t *testing.T) {
	t.Parallel()
	f := fixture24(t)
	o := f.obligation(t)
	now := float64(1700000000)
	checkSelection(t, f, o, "", &now)
	checkSelection(t, f, o, "01supervisor-task", &now)
	if _, err := f.s.DB.Exec("INSERT INTO recipient_lifecycle (task_id,deliverable,withhold_reason,observed_at) VALUES (?,?,?,?)", "01supervisor-task", "yes", nil, f.at); err != nil {
		t.Fatal(err)
	}
	checkSelection(t, f, o, "01supervisor-task", &now)
	later := now + 961
	checkSelection(t, f, o, "01supervisor-task", &later)
	future := now - 61
	checkSelection(t, f, o, "01supervisor-task", &future)
	skew := now - 5
	checkSelection(t, f, o, "01supervisor-task", &skew)
	checkSelection(t, f, o, "01supervisor-task", nil)
	if _, err := f.s.DB.Exec("UPDATE recipient_lifecycle SET deliverable='no',withhold_reason='recipient_paused' WHERE task_id='01supervisor-task'"); err != nil {
		t.Fatal(err)
	}
	checkSelection(t, f, o, "01supervisor-task", &now)
}
