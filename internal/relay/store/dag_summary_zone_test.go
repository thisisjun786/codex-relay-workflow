package store

import (
	"fmt"
	"strings"
	"testing"
)

// CRW-283: the project summary outbox (docs/relay/dag-outbox.md). The table is the zone's queue of the Linear summaries a plan's parent
// writes with its own connector. These tests drive the schema with raw SQL, as an operator's sqlite3 would, so what they show is what the
// zone itself enforces and not what the writer happens to do: an older summary is never appended after a newer one, never claimed or
// confirmed once a newer one exists, and an entry that was confirmed or superseded never changes.

const summaryPlan = "INSERT INTO dag_plans VALUES ('plan-1','PRJ-A','task-a','2026-10-02T00:00:00Z')"

// summaryInsert is one entry of the outbox, written column by column so a test names only what it varies.
func summaryInsert(plan, document string, revision, seq int, state string) string {
	token, claimedBy, claimedAt, confirmedAt := "NULL", "NULL", "NULL", "NULL"
	switch state {
	case "claimed":
		token, claimedBy, claimedAt = "'tok-1'", "'task-a'", "'2026-10-02T00:00:01Z'"
	case "confirmed":
		confirmedAt = "'2026-10-02T00:00:02Z'"
	}
	return fmt.Sprintf("INSERT INTO dag_summary_outbox (summary_id, plan_id, project_key, document, plan_revision, seq, subject_digest, state_digest, summary, summary_sha256, state, attempts, "+
		"claim_token, claimed_by, claimed_at, confirmed_at, enqueued_by, created_at, updated_at) VALUES ('sum-%s-%s-%d','%s','PRJ-A','%s',%d,%d,'%s','%s','a summary','%s','%s',0,%s,%s,%s,%s,'task-a','2026-10-02T00:00:00Z','2026-10-02T00:00:00Z')",
		plan, document, seq, plan, document, revision, seq, zoneDigest, zoneDigest, zoneDigest, state, token, claimedBy, claimedAt, confirmedAt)
}

func summaryID(plan, document string, seq int) string {
	return fmt.Sprintf("sum-%s-%s-%d", plan, document, seq)
}

func summaryUpdate(plan, document string, seq int, set string) string {
	return fmt.Sprintf("UPDATE dag_summary_outbox SET %s WHERE summary_id = '%s'", set, summaryID(plan, document, seq))
}

// An entry is appended pending, with the next sequence number of its stream (a plan and a document) and a plan revision that does not go
// back; a second open entry of the stream is refused, so the older ones are superseded before a newer one is appended.
func TestDAGSummaryOutboxEntriesAreAppendedInOrder(t *testing.T) {
	t.Parallel()
	db := zoneOpenedDB(t)
	zoneRefuses(t, db, "an entry of a plan that was never created", summaryInsert("plan-1", "doc-a", 1, 1, "pending"))
	zoneMustExec(t, db, summaryPlan, summaryInsert("plan-1", "doc-a", 5, 1, "pending"))
	zoneRefuses(t, db, "the same sequence number again", strings.Replace(summaryInsert("plan-1", "doc-a", 5, 1, "pending"), "sum-plan-1-doc-a-1", "sum-other", 1))
	zoneRefuses(t, db, "a sequence number that skips one", summaryInsert("plan-1", "doc-a", 5, 3, "pending"))
	zoneRefuses(t, db, "an entry that is not appended pending", summaryInsert("plan-1", "doc-a", 5, 2, "claimed"))
	zoneRefuses(t, db, "a second open entry while the first is still open", summaryInsert("plan-1", "doc-a", 6, 2, "pending"))
	zoneMustExec(t, db, summaryUpdate("plan-1", "doc-a", 1, "state = 'superseded', updated_at = '2026-10-02T00:00:03Z'"))
	zoneRefuses(t, db, "a plan revision that goes back", summaryInsert("plan-1", "doc-a", 4, 2, "pending"))
	zoneMustExec(t, db, summaryInsert("plan-1", "doc-a", 5, 2, "pending"))
	// another document of the plan, and another plan, are streams of their own and start at 1
	zoneMustExec(t, db, summaryInsert("plan-1", "doc-b", 1, 1, "pending"), "INSERT INTO dag_plans VALUES ('plan-2','PRJ-A','task-a','2026-10-02T00:00:00Z')", summaryInsert("plan-2", "doc-a", 1, 1, "pending"))
}

// The legal moves of an open entry, and what the zone refuses: only confirmed and superseded are final, a claim holds a token and nothing else does,
// identity never changes, and nothing is deleted.
func TestDAGSummaryOutboxTransitions(t *testing.T) {
	t.Parallel()
	db := zoneOpenedDB(t)
	zoneMustExec(t, db, summaryPlan, summaryInsert("plan-1", "doc-a", 1, 1, "pending"))
	update := func(set string) string { return summaryUpdate("plan-1", "doc-a", 1, set) }
	claim := "state = 'claimed', claim_token = 'tok-1', claimed_by = 'task-a', claimed_at = '2026-10-02T00:00:01Z'"
	zoneRefuses(t, db, "a confirmation of an entry nobody claimed", update("state = 'confirmed', confirmed_at = '2026-10-02T00:00:02Z'"))
	zoneRefuses(t, db, "a claim without a token", update("state = 'claimed'"))
	zoneRefuses(t, db, "a token on an entry that is not claimed", update("claim_token = 'tok-x'"))
	zoneMustExec(t, db, update(claim))
	zoneMustExec(t, db, update("claim_token = 'tok-2'")) // a claim again rotates the token: the old one is fenced
	zoneMustExec(t, db, update("state = 'pending', attempts = 1, last_error = 'the write failed', claim_token = NULL"))
	zoneMustExec(t, db, update(claim))
	zoneMustExec(t, db, update("state = 'failed', attempts = 8, claim_token = NULL"))
	zoneRefuses(t, db, "a claim of a failed entry (it is retried first)", update(claim))
	zoneMustExec(t, db, update("state = 'pending', attempts = 0"))
	zoneMustExec(t, db, update(claim))
	zoneRefuses(t, db, "a confirmation that keeps the token", update("state = 'confirmed', confirmed_at = '2026-10-02T00:00:02Z'"))
	zoneRefuses(t, db, "a confirmation with no time", update("state = 'confirmed', claim_token = NULL"))
	zoneRefuses(t, db, "a change of the summary text", update("summary = 'another summary'"))
	zoneRefuses(t, db, "a change of the document", update("document = 'doc-z'"))
	zoneRefuses(t, db, "a change of the sequence number", update("seq = 9"))
	zoneRefuses(t, db, "a change of who enqueued it", update("enqueued_by = 'task-z'"))
	zoneMustExec(t, db, update("state = 'confirmed', claim_token = NULL, confirmed_at = '2026-10-02T00:00:02Z', readback = 'block'"))
	zoneRefuses(t, db, "a change of a confirmed entry", update("last_error = 'x'"))
	zoneRefuses(t, db, "a confirmed entry that becomes pending again", update("state = 'pending', confirmed_at = NULL"))
	zoneRefuses(t, db, "a deletion", "DELETE FROM dag_summary_outbox")

	zoneMustExec(t, db, summaryInsert("plan-1", "doc-a", 1, 2, "pending"))
	zoneMustExec(t, db, summaryUpdate("plan-1", "doc-a", 2, "state = 'superseded'"))
	zoneRefuses(t, db, "a superseded entry that is claimed", summaryUpdate("plan-1", "doc-a", 2, "state = 'claimed', claim_token = 'tok-3'"))
	zoneRefuses(t, db, "a change of a superseded entry", summaryUpdate("plan-1", "doc-a", 2, "attempts = 5"))
}

// The newest-sibling rule is a second line behind the single open entry: with the open-entry index out of the way (a store a future build
// alters), an entry that has a newer sibling still cannot be claimed or confirmed.
func TestDAGSummaryOutboxOlderEntryIsNeverClaimedOrConfirmedOverANewerOne(t *testing.T) {
	t.Parallel()
	db := zoneOpenedDB(t)
	zoneMustExec(t, db, summaryPlan, summaryInsert("plan-1", "doc-a", 1, 1, "pending"), "DROP INDEX dag_summary_outbox_open", summaryInsert("plan-1", "doc-a", 1, 2, "pending"))
	zoneRefuses(t, db, "a claim of the older entry", summaryUpdate("plan-1", "doc-a", 1, "state = 'claimed', claim_token = 'tok-1'"))
	zoneMustExec(t, db, summaryUpdate("plan-1", "doc-a", 2, "state = 'claimed', claim_token = 'tok-1'"))
	zoneMustExec(t, db, summaryUpdate("plan-1", "doc-a", 2, "state = 'confirmed', claim_token = NULL, confirmed_at = '2026-10-02T00:00:02Z'"))
}
