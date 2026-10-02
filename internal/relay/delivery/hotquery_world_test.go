package delivery

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// CRW-294: the delivery path's hot queries run on every daemon tick, so what they cost is what
// an idle store costs. The world here is a store of the size the issue measured: a final event
// for every delivery and two attempts for every event, spread over a few hundred relationships
// and a few dozen parents. Almost all of it is history (acknowledged deliveries, settled
// attempts, bound anchors); what a tick looks for is the handful of rows that are not.

const (
	hotRelationships = 400
	hotParents       = 40
	// hotEpoch is the stamp of event 0 in seconds; event i is first seen hotSpacing seconds after
	// event i-1, so the store holds hotSpacing*events seconds of history ending at its clock.
	hotEpoch   = 1_700_000_000
	hotSpacing = 30
	// hotStoreEnv names a directory the benchmarks keep their stores in instead of a temporary
	// one, so the same rows can be opened by the sqlite3 shell (EXPLAIN QUERY PLAN beside the
	// driver's).
	hotStoreEnv = "CRW_HOTQUERY_DIR"
)

// hotPad is expr as a zero-padded decimal of width digits, in SQL.
func hotPad(expr string, width int) string {
	return fmt.Sprintf("substr('%s' || (%s), -%d)", strings.Repeat("0", width), expr, width)
}

// hotStamp is the send stamp (microsecond ISO text) of the instant expr seconds after the Unix epoch.
func hotStamp(expr string) string {
	return "strftime('%Y-%m-%dT%H:%M:%S', " + expr + ", 'unixepoch') || '.000000+00:00'"
}

// hotSeed is the statements that fill an empty store with the given number of events, one
// delivery per event and perEvent attempts of each, in event order:
//
//   - relationship j (of hotRelationships) has parent j mod hotParents; every 25th is archived;
//   - event i belongs to relationship i mod hotRelationships and is final;
//   - the delivery of an event is a completion to its relationship's parent, except that every
//     revisionEvery-th event of a relationship is a revision request to the child; one in seven
//     is dispatched and the rest acknowledged, so nothing is waiting;
//   - attempt 1 of an event found its recipient busy (one in fifty held uncertain, which a
//     delivery that moved on no longer answers for), attempt 2 was dispatched;
//   - every relationship has one generation, with its anchor bound.
func hotSeed(events, perEvent, revisionEvery int) []string {
	n := "WITH RECURSIVE n(i) AS (SELECT 0 UNION ALL SELECT i + 1 FROM n WHERE i < %d) "
	rel := "'rel-' || " + hotPad(fmt.Sprintf("i %% %d", hotRelationships), 4)
	ev := "'ev-' || " + hotPad("i", 6)
	at := func(offset int) string {
		return hotStamp(fmt.Sprintf("%d + i * %d + %d", hotEpoch, hotSpacing, offset))
	}
	stmts := []string{
		fmt.Sprintf(n, hotRelationships-1) + "INSERT INTO relationships (relationship_id, issue_key, status, parent_task_id, parent_host_id, child_task_id, child_host_id, execution_generation, artifact_roots, allowed_recipients, created_at, updated_at) " +
			"SELECT 'rel-' || " + hotPad("i", 4) + ", 'HOT-' || i, CASE WHEN i % 25 = 24 THEN 'archived' ELSE 'active' END, 'parent-' || " + hotPad(fmt.Sprintf("i %% %d", hotParents), 2) + ", 'host-1', 'child-' || " + hotPad("i", 4) + ", 'host-1', 1, '[]', '[]', " + at(0) + ", " + at(0) + " FROM n",
		fmt.Sprintf(n, hotRelationships-1) + "INSERT INTO generations (relationship_id, execution_generation, dispatch_request_id, anchor_state, dispatch_turn_id, opened_at, bound_at) " +
			"SELECT 'rel-' || " + hotPad("i", 4) + ", 1, 'dispatch-' || " + hotPad("i", 4) + ", 'bound', 'anchor-' || " + hotPad("i", 4) + ", " + at(0) + ", " + at(1) + " FROM n",
		fmt.Sprintf(n, events-1) + "INSERT INTO events (event_id, relationship_id, execution_generation, revision_hash, outcome, producer, turn_thread_id, turn_id, turn_status, receipt, stage, first_seen_at, last_seen_at) " +
			"SELECT " + ev + ", " + rel + ", 1, 'rev-' || " + hotPad("i", 6) + ", 'ready_for_review', 'child', 'child-' || " + hotPad(fmt.Sprintf("i %% %d", hotRelationships), 4) + ", 'turn-' || " + hotPad("i", 6) + ", 'completed', '{}', 'final', " + at(0) + ", " + at(0) + " FROM n",
		fmt.Sprintf(n, events-1) + "INSERT INTO deliveries (event_id, relationship_id, kind, recipient_task_id, recipient_thread_id, state, attempt_count, next_eligible_at, dispatch_turn_id, created_at, updated_at) " +
			"SELECT " + ev + ", " + rel + ", kind, recipient, recipient, state, 2, NULL, 'turn-' || " + hotPad("i", 6) + ", " + at(0) + ", " + at(1) + " FROM (SELECT i," +
			" CASE WHEN (i / " + fmt.Sprint(hotRelationships) + ") % " + fmt.Sprint(revisionEvery) + " = 0 THEN 'revision_request' ELSE 'completion_event' END AS kind," +
			" CASE WHEN (i / " + fmt.Sprint(hotRelationships) + ") % " + fmt.Sprint(revisionEvery) + " = 0 THEN 'child-' || " + hotPad(fmt.Sprintf("i %% %d", hotRelationships), 4) + " ELSE 'parent-' || " + hotPad(fmt.Sprintf("(i %% %d) %% %d", hotRelationships, hotParents), 2) + " END AS recipient," +
			" CASE WHEN i % 7 = 0 THEN 'dispatched' ELSE 'acknowledged' END AS state FROM n)",
	}
	for k := 1; k <= perEvent; k++ {
		state := "'dispatched'"
		if k == 1 {
			state = "CASE WHEN i % 50 = 0 THEN 'held_uncertain' ELSE 'deferred_busy' END"
		}
		stmts = append(stmts, fmt.Sprintf(n, events-1)+"INSERT INTO attempts (request_id, event_id, attempt_no, kind, internal_state, state, sent_at, observed_at) "+
			fmt.Sprintf("SELECT 'req-' || %s || '-%d', %s, %d, 'completion_event', 'settled', %s, %s, %s FROM n", hotPad("i", 6), k, ev, k, state, at(k-perEvent), at(k-perEvent)))
	}
	return stmts
}

// hotWorld is a seeded store and the services that read it.
type hotWorld struct {
	tb     testing.TB
	ctx    context.Context
	store  *store.Store
	d      *Service
	rc     *Reconciler
	ack    *Ack
	events int
	// now is just after the newest event: the instant the daemon's next tick runs at.
	now float64
}

// newHotWorld seeds a store of the given size (hotSeed) in a temporary directory; a benchmark
// keeps it instead in a directory of its own name under the one hotStoreEnv names when that is
// set (a store directory holds one store).
func newHotWorld(tb testing.TB, name string, events, perEvent, revisionEvery int) *hotWorld {
	tb.Helper()
	dir := ""
	if _, ok := tb.(*testing.B); ok {
		dir = os.Getenv(hotStoreEnv)
	}
	if dir == "" {
		dir = tb.TempDir()
	}
	path := filepath.Join(dir, name, "relay.sqlite3")
	ctx := context.Background()
	s, err := store.Open(ctx, path, "")
	if err != nil {
		tb.Fatal(err)
	}
	tb.Cleanup(func() { _ = s.Close() })
	for _, stmt := range hotSeed(events, perEvent, revisionEvery) {
		if _, err := execSQL(ctx, s, stmt); err != nil {
			tb.Fatalf("%v\n%s", err, stmt)
		}
	}
	now := float64(hotEpoch + events*hotSpacing + 60)
	clock := &FakeClock{T: now}
	d := NewService(s, clock)
	return &hotWorld{tb: tb, ctx: ctx, store: s, d: d, rc: NewReconciler(d), ack: NewAck(d), events: events, now: now}
}

func (w *hotWorld) exec(query string, args ...any) int64 {
	w.tb.Helper()
	n, err := execSQL(w.ctx, w.store, query, args...)
	if err != nil {
		w.tb.Fatal(err)
	}
	return n
}
