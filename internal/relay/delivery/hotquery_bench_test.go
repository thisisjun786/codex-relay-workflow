package delivery

import (
	"strings"
	"testing"
)

// The statements of the three hot queries as they were before CRW-294, kept as written so the
// benchmarks can run them beside the current ones and the equivalence tests can hold the current
// ones to the rows and order these returned.
const (
	// legacyEligibleBase is eligibleBase.
	legacyEligibleBase = " WHERE d.state IN (?,?,?) AND d.hold_reason IS NULL AND (d.next_eligible_at IS NULL OR d.next_eligible_at <= ?) AND r.status = 'active' AND r.superseded_by IS NULL AND e.stage = 'final'"
	// legacyOpenParentsSQL is OpenParents' statement; its arguments are HeldUncertain, HeldUncertain, Sending.
	legacyOpenParentsSQL = "SELECT DISTINCT r.parent_task_id AS parent_task_id FROM attempts a JOIN deliveries d ON d.event_id = a.event_id JOIN relationships r ON r.relationship_id = d.relationship_id" +
		" WHERE (a.internal_state = 'in_flight' OR (a.state = ? AND d.state IN (?, ?))) ORDER BY r.parent_task_id"
	// legacyPendingAnchorsSQL is BindPendingAnchors' read; its arguments are Revision, Dispatched, Acknowledged, "anchor_pending", 50.
	legacyPendingAnchorsSQL = "SELECT d.event_id FROM deliveries d JOIN events e ON e.event_id = d.event_id JOIN generations g ON g.relationship_id = e.relationship_id AND g.execution_generation = e.execution_generation WHERE d.kind = ? AND d.state IN (?,?) AND d.dispatch_turn_id IS NOT NULL AND g.anchor_state = ? ORDER BY d.updated_at LIMIT ?"
)

// legacyEligibleParents is EligibleParents with eligibleBase as it was.
func (w *hotWorld) legacyEligibleParents() []string {
	w.tb.Helper()
	join, where, args := w.d.eligibility(w.now)
	if !strings.Contains(where, eligibleBase) {
		w.tb.Fatalf("eligibility no longer starts from eligibleBase: %q", where)
	}
	where = strings.Replace(where, eligibleBase, legacyEligibleBase, 1)
	rows, err := all(w.ctx, w.store, "SELECT DISTINCT r.parent_task_id AS parent_task_id"+dueFrom+join+where+" ORDER BY r.parent_task_id", args...)
	if err != nil {
		w.tb.Fatal(err)
	}
	var out []string
	for _, r := range rows {
		out = append(out, r.S("parent_task_id"))
	}
	return out
}

func (w *hotWorld) legacyOpenParents() []string {
	w.tb.Helper()
	rows, err := all(w.ctx, w.store, legacyOpenParentsSQL, HeldUncertain, HeldUncertain, Sending)
	if err != nil {
		w.tb.Fatal(err)
	}
	out := []string{}
	for _, r := range rows {
		out = append(out, r.S("parent_task_id"))
	}
	return out
}

func (w *hotWorld) legacyPendingAnchors() []string {
	w.tb.Helper()
	rows, err := all(w.ctx, w.store, legacyPendingAnchorsSQL, Revision, Dispatched, Acknowledged, "anchor_pending", 50)
	if err != nil {
		w.tb.Fatal(err)
	}
	var out []string
	for _, r := range rows {
		out = append(out, r.S("event_id"))
	}
	return out
}

// makeBusy gives each hot query a few rows to find: twelve of the newest deliveries are queued
// again (to a handful of parents), two attempts are in flight and one delivery is held uncertain,
// and three generations wait for their anchor.
func (w *hotWorld) makeBusy() {
	w.tb.Helper()
	w.exec("UPDATE deliveries SET state = 'queued', dispatch_turn_id = NULL WHERE event_id IN (SELECT event_id FROM deliveries WHERE kind = 'completion_event' ORDER BY event_id DESC LIMIT 12)")
	w.exec("UPDATE attempts SET internal_state = 'in_flight' WHERE request_id IN ('req-000010-2', 'req-000011-2')")
	w.exec("UPDATE deliveries SET state = 'held_uncertain' WHERE event_id = 'ev-000050'")
	w.exec("UPDATE generations SET anchor_state = 'anchor_pending', dispatch_turn_id = 'turn', bound_at = NULL WHERE relationship_id IN (SELECT relationship_id FROM generations ORDER BY relationship_id LIMIT 3)")
}

const (
	// hotBenchEvents is the issue's measure: 80,000 final events, and so 160,000 attempts.
	hotBenchEvents = 80_000
	// hotRevisionEvery makes one event of twenty of a relationship a revision request.
	hotRevisionEvery = 20
)

// hotRun is one benchmark case.
func hotRun(b *testing.B, name string, f func()) {
	b.Run(name, func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			f()
		}
	})
}

// hotCases times the three queries a daemon tick runs whether or not anything is due, and the
// same statements as they were before CRW-294 (the _legacy cases) on the same rows.
func hotCases(b *testing.B, w *hotWorld) {
	hotRun(b, "EligibleParents", func() {
		if _, err := w.d.EligibleParents(w.ctx, w.now); err != nil {
			b.Fatal(err)
		}
	})
	hotRun(b, "EligibleParents_legacy", func() { w.legacyEligibleParents() })
	hotRun(b, "OpenParents", func() {
		if _, err := w.rc.OpenParents(w.ctx); err != nil {
			b.Fatal(err)
		}
	})
	hotRun(b, "OpenParents_legacy", func() { w.legacyOpenParents() })
}

// BenchmarkHotQueriesQuiet is an idle store of 80,000 final events and 160,000 attempts: nothing
// is due, open or pending, so every query scans for nothing and finds it. BindPendingAnchors
// binds what it finds, so it is timed here, where it finds nothing; BenchmarkHotQueriesBusy
// times its read.
//
//	go test ./internal/relay/delivery -run '^$' -bench HotQueries -benchtime 50x
func BenchmarkHotQueriesQuiet(b *testing.B) {
	w := newHotWorld(b, "quiet", hotBenchEvents, 2, hotRevisionEvery)
	hotCases(b, w)
	hotRun(b, "BindPendingAnchors", func() {
		if _, err := w.ack.BindPendingAnchors(w.ctx); err != nil {
			b.Fatal(err)
		}
	})
}

// BenchmarkHotQueriesBusy is the same store with a few rows for each query to find (makeBusy).
func BenchmarkHotQueriesBusy(b *testing.B) {
	w := newHotWorld(b, "busy", hotBenchEvents, 2, hotRevisionEvery)
	w.makeBusy()
	if len(w.legacyEligibleParents()) == 0 || len(w.legacyOpenParents()) == 0 || len(w.legacyPendingAnchors()) == 0 {
		b.Fatal("the busy store has nothing to find")
	}
	hotCases(b, w)
	hotRun(b, "PendingAnchorsRead", func() {
		if _, err := w.ack.pendingAnchors(w.ctx); err != nil {
			b.Fatal(err)
		}
	})
	hotRun(b, "PendingAnchorsRead_legacy", func() { w.legacyPendingAnchors() })
}
