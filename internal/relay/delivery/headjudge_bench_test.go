package delivery

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// CRW-416: a claim judges the head of the generation its event belongs to, and the head of a
// generation is a statement about every reviewable revision in it (a fork, a cycle or a declared
// predecessor nobody holds anywhere in the generation makes the whole generation ambiguous). The
// world here is one relationship whose generation holds the given number of reviewable revisions
// and one queued completion delivery, the shape the CRW-270 measurement used.

// headShape is how the revisions of the generation declare their predecessors.
type headShape string

const (
	// shapeLoose declares nothing: every revision stands alone (an undeclared lineage row each), so
	// the generation is a fork and has no head (the CRW-270 synthetic world).
	shapeLoose headShape = "loose"
	// shapeChain declares one chain: revision i supersedes revision i-1, so the generation has one
	// head, the last revision.
	shapeChain headShape = "chain"
)

const (
	headRelationship = "rel-head"
	headRecipient    = "parent-head"
	headEpoch        = 1_700_000_000
)

// headEvent names the event of revision i of the world. An event id is 32 hexadecimal digits,
// which RequestID insists on, and sorts as i does.
func headEvent(i int) string { return fmt.Sprintf("%032x", i+1) }

// headWorld is a seeded store and a service that claims on it.
type headWorld struct {
	tb    testing.TB
	ctx   context.Context
	store *store.Store
	d     *Service
	// tip is the last revision of the generation: its event holds the one queued delivery.
	tip string
	now float64
}

func newHeadWorld(tb testing.TB, shape headShape, events int) *headWorld {
	tb.Helper()
	ctx := context.Background()
	s, err := store.Open(ctx, filepath.Join(tb.TempDir(), "relay.sqlite3"), "")
	if err != nil {
		tb.Fatal(err)
	}
	tb.Cleanup(func() { _ = s.Close() })
	stamp := hotStamp(fmt.Sprint(headEpoch))
	n := "WITH RECURSIVE n(i) AS (SELECT 0 UNION ALL SELECT i + 1 FROM n WHERE i < %d) "
	ev := "printf('%032x', i + 1)"
	rev := "'rev-' || " + hotPad("i", 6)
	stmts := []string{
		"INSERT INTO relationships (relationship_id, issue_key, status, parent_task_id, parent_host_id, child_task_id, child_host_id, execution_generation, artifact_roots, allowed_recipients, created_at, updated_at) VALUES ('" + headRelationship + "', 'HEAD-1', 'active', '" + headRecipient + "', 'host-1', 'child-head', 'host-1', 1, '[]', '[]', " + stamp + ", " + stamp + ")",
		"INSERT INTO generations (relationship_id, execution_generation, dispatch_request_id, anchor_state, dispatch_turn_id, opened_at, bound_at) VALUES ('" + headRelationship + "', 1, 'dispatch-head', 'bound', 'anchor-head', " + stamp + ", " + stamp + ")",
		fmt.Sprintf(n, events-1) + "INSERT INTO events (event_id, relationship_id, execution_generation, revision_hash, outcome, producer, turn_thread_id, turn_id, turn_status, receipt, stage, first_seen_at, last_seen_at) " +
			"SELECT " + ev + ", '" + headRelationship + "', 1, " + rev + ", 'ready_for_review', 'child', 'child-head', 'turn-' || " + hotPad("i", 6) + ", 'completed', '{}', 'final', " + stamp + ", " + stamp + " FROM n",
	}
	// The product writes a lineage row for every reviewable revision, undeclared (no predecessor) or
	// declared, in the transaction that stores the event.
	supersedes, declaredBy := "NULL", "'undeclared'"
	if shape == shapeChain {
		supersedes = "CASE WHEN i > 0 THEN 'rev-' || " + hotPad("i - 1", 6) + " END"
		declaredBy = "CASE WHEN i > 0 THEN 'child_declared' ELSE 'undeclared' END"
	}
	stmts = append(stmts, fmt.Sprintf(n, events-1)+"INSERT INTO revision_lineage (relationship_id, execution_generation, event_id, revision_hash, supersedes_hash, declared_by, recorded_at) "+
		"SELECT '"+headRelationship+"', 1, "+ev+", "+rev+", "+supersedes+", "+declaredBy+", "+stamp+" FROM n")
	tip := headEvent(events - 1)
	stmts = append(stmts, "INSERT INTO deliveries (event_id, relationship_id, kind, recipient_task_id, recipient_thread_id, state, attempt_count, next_eligible_at, created_at, updated_at) VALUES ('"+tip+"', '"+headRelationship+"', 'completion_event', '"+headRecipient+"', '"+headRecipient+"', 'queued', 0, NULL, "+stamp+", "+stamp+")")
	for _, stmt := range stmts {
		if _, err := execSQL(ctx, s, stmt); err != nil {
			tb.Fatalf("%v\n%s", err, stmt)
		}
	}
	now := float64(headEpoch + 3600)
	return &headWorld{tb: tb, ctx: ctx, store: s, d: NewService(s, &FakeClock{T: now}), tip: tip, now: now}
}

// requeue puts the delivery back to where it was before a claim took it.
func (w *headWorld) requeue() {
	w.tb.Helper()
	for _, stmt := range []string{
		"DELETE FROM attempt_messages WHERE event_id = '" + w.tip + "'",
		"DELETE FROM attempts WHERE event_id = '" + w.tip + "'",
		"DELETE FROM recipient_rate",
		"UPDATE deliveries SET state = 'queued', lease_owner = NULL, lease_until = NULL, attempt_count = 0 WHERE event_id = '" + w.tip + "'",
	} {
		if _, err := execSQL(w.ctx, w.store, stmt); err != nil {
			w.tb.Fatal(err)
		}
	}
}

// claim is one claim of the world's delivery.
func (w *headWorld) claim() (claimed, error) {
	return w.d.claim(w.ctx, w.tip, w.now, "bench", headRecipient)
}

// BenchmarkClaimHead times one claim of a queued completion whose generation holds the given
// number of reviewable revisions of one relationship, each claim from a delivery put back to
// queued (outside the timer).
//
//	go test ./internal/relay/delivery -run '^$' -bench ClaimHead -benchtime 3x
func BenchmarkClaimHead(b *testing.B) {
	for _, shape := range []headShape{shapeLoose, shapeChain} {
		for _, events := range []int{100, 1_000, 10_000} {
			b.Run(fmt.Sprintf("shape=%s/events=%d", shape, events), func(b *testing.B) {
				w := newHeadWorld(b, shape, events)
				b.ReportAllocs()
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					b.StopTimer()
					w.requeue()
					b.StartTimer()
					if _, err := w.claim(); err != nil {
						b.Fatal(err)
					}
				}
			})
		}
	}
}
