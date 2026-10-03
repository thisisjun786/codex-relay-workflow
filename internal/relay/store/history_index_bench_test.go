package store

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
)

// CRW-301: the history indexes. The seed is a store the way a long-lived relay holds one: events and
// deliveries with two settled attempts each, most of them acknowledged, over a few hundred relationships
// and a few dozen parents, with the supervisor's messages, attempts and Linear writes beside them. The
// default size is the one the issue measured (80,000 events, 160,000 attempts); CRW_HISTORY_EVENTS changes it.
//
//	go test ./internal/relay/store -run '^$' -bench 'HistoryIndexes|OpenBuilds' -benchtime 20x
const (
	historyRelationships = 400
	historyParents       = 40
	historyEpoch         = 1_700_000_000
	historySpacing       = 30
)

func historyEvents() int {
	var n int
	if _, err := fmt.Sscan(os.Getenv("CRW_HISTORY_EVENTS"), &n); err != nil || n < 100 {
		return 80_000
	}
	return n
}

func historyPad(expr string, width int) string {
	return fmt.Sprintf("substr('%s' || (%s), -%d)", strings.Repeat("0", width), expr, width)
}

func historyStamp(expr string) string {
	return "strftime('%Y-%m-%dT%H:%M:%S', " + expr + ", 'unixepoch') || '.000000+00:00'"
}

// historySeed is the statements that fill an empty store with the given number of events.
func historySeed(events int) []string {
	n := func(rows int) string {
		return fmt.Sprintf("WITH RECURSIVE n(i) AS (SELECT 0 UNION ALL SELECT i + 1 FROM n WHERE i < %d) ", rows-1)
	}
	at := func(spacing, offset int) string {
		return historyStamp(fmt.Sprintf("%d + i * %d + %d", historyEpoch, spacing, offset))
	}
	rel := func(expr string) string { return "'rel-' || " + historyPad(expr, 4) }
	parent := func(expr string) string {
		return "'parent-' || " + historyPad(fmt.Sprintf("(%s) %% %d", expr, historyParents), 2)
	}
	ev := func(expr string) string { return "'ev-' || " + historyPad(expr, 6) }
	messages := events / 8
	out := []string{
		n(historyRelationships) + "INSERT INTO relationships (relationship_id, issue_key, status, parent_task_id, parent_host_id, child_task_id, child_host_id, execution_generation, artifact_roots, allowed_recipients, created_at, updated_at) " +
			"SELECT " + rel("i") + ", 'HIST-' || i, 'active', " + parent("i") + ", 'host', 'child-' || i, 'host', 1, '[]', '[]', " + at(0, 0) + ", " + at(0, 0) + " FROM n",
		n(historyRelationships) + "INSERT INTO generations (relationship_id, execution_generation, dispatch_request_id, anchor_state, dispatch_turn_id, opened_at, bound_at) " +
			"SELECT " + rel("i") + ", 1, 'dispatch-' || " + historyPad("i", 4) + ", 'bound', 'anchor-' || i, " + at(0, 0) + ", " + at(0, 1) + " FROM n",
		n(events) + "INSERT INTO events (event_id, relationship_id, execution_generation, revision_hash, outcome, producer, turn_thread_id, turn_id, turn_status, receipt, stage, first_seen_at, last_seen_at) " +
			"SELECT " + ev("i") + ", " + rel(fmt.Sprintf("i %% %d", historyRelationships)) + ", 1, 'rev-' || i, 'ready_for_review', 'child', 'child-' || i, 'turn-' || i, 'completed', '{}', 'final', " + at(historySpacing, 0) + ", " + at(historySpacing, 0) + " FROM n",
		n(events) + "INSERT INTO deliveries (event_id, relationship_id, kind, recipient_task_id, recipient_thread_id, state, attempt_count, created_at, updated_at) " +
			"SELECT " + ev("i") + ", " + rel(fmt.Sprintf("i %% %d", historyRelationships)) + ", 'completion_event', " + parent(fmt.Sprintf("i %% %d", historyRelationships)) + ", 'thread', CASE WHEN i % 7 = 0 THEN 'dispatched' ELSE 'acknowledged' END, 2, " + at(historySpacing, 0) + ", " + at(historySpacing, 1) + " FROM n",
	}
	for k := 1; k <= 2; k++ {
		out = append(out, n(events)+fmt.Sprintf("INSERT INTO attempts (request_id, event_id, attempt_no, kind, internal_state, state, sent_at, observed_at) SELECT 'req-' || %s || '-%d', %s, %d, 'completion_event', 'settled', 'dispatched', %s, %s FROM n", historyPad("i", 6), k, ev("i"), k, at(historySpacing, k), at(historySpacing, k)))
	}
	return append(out,
		// every acknowledged delivery has its acknowledgement; the newest forty have not been verified
		n(events)+"INSERT INTO acks (event_id, record, ack_turn_id, accepted, verified, ack_at) SELECT "+ev("i")+", '{}', 'ackturn-' || i, 1, CASE WHEN i >= "+fmt.Sprint(events-40)+" THEN 'unverified_turn' ELSE 'verified' END, "+at(historySpacing, 2)+" FROM n WHERE i % 7 <> 0",
		n(events)+"INSERT INTO ack_evidence (event_id, tier, attempts, last_reason, observed_at) SELECT "+ev("i")+", 'turn', 1, 'delivery_unconfirmed', "+at(historySpacing, 2)+" FROM n WHERE i % 7 <> 0 AND i >= "+fmt.Sprint(events-40),
		// one supervisor message, with a Linear write, for every eighth event; each message sent once or twice
		n(messages)+"INSERT INTO supervisor_messages (message_id, obligation_id, obligation_kind, relationship_id, purpose, kind, sender_task_id, recipient_task_id, subject, packet, state, staged_at, updated_at, event_id) "+
			"SELECT 'msg-' || "+historyPad("i", 6)+", 'obl-' || "+historyPad("i", 6)+", 'work_report', "+rel(fmt.Sprintf("(i * 8) %% %d", historyRelationships))+", 'report', 'work_report', 'sup', "+parent(fmt.Sprintf("(i * 8) %% %d", historyRelationships))+", 's', '{}', 'dispatched', "+at(historySpacing*8, 0)+", "+at(historySpacing*8, 0)+", "+ev("i * 8")+" FROM n",
		n(messages*3/2)+"INSERT INTO supervisor_attempts (request_id, message_id, attempt_no, message, state, send_attempted, retry_safe, record, sent_at, transport_started_at, observed_at) "+
			"SELECT 'sreq-' || "+historyPad("i", 6)+", 'msg-' || "+historyPad(fmt.Sprintf("i %% %d", messages), 6)+", i / "+fmt.Sprint(messages)+" + 1, 'm', 'settled', CASE WHEN i % 3 = 0 THEN 'no' ELSE 'yes' END, i % 3 = 0, '{}', "+historyStamp(fmt.Sprintf("%d + (i %% %d) * %d + (i / %d) * 7", historyEpoch, messages, historySpacing*8, messages))+", "+historyStamp(fmt.Sprintf("%d + (i %% %d) * %d + (i / %d) * 7", historyEpoch, messages, historySpacing*8, messages))+", "+at(historySpacing*8, 0)+" FROM n",
		n(messages)+"INSERT INTO sync_outbox (sync_id, relationship_id, issue_key, target, target_ref, subject_kind, event_id, identity_digest, summary, state, created_at, updated_at) "+
			"SELECT 'sync-' || "+historyPad("i", 6)+", "+rel(fmt.Sprintf("(i * 8) %% %d", historyRelationships))+", 'HIST-1', 'coordination_document', 'doc', 'verdict', "+ev("i * 8")+", 'digest', 'summary', 'confirmed', "+at(historySpacing*8, 0)+", "+at(historySpacing*8, 0)+" FROM n",
		n(2000)+"INSERT INTO managed_start_requests (request_id, issue_key, request_fingerprint, fingerprint_version, workspace, marker_root, socket_identity, create_request_id, dispatch_request_id, state, revision, created_at, updated_at) "+
			"SELECT 'ms-' || "+historyPad("i", 5)+", 'ISS-' || i, 'fp', 'v1', '/w', '/m', 'sock', 'create-' || i, 'dispatch-' || "+historyPad("i", 4)+", 'attached', 1, "+at(60, 0)+", "+at(60, 0)+" FROM n",
	)
}

// historyIndexDDL is the CREATE statement the embedded schema script gives the index, so a benchmark
// builds the index the store ships and not a copy of it.
func historyIndexDDL(tb testing.TB, name string) string {
	tb.Helper()
	script, err := schema.ReadFile("relay-sqlite.sql")
	if err != nil {
		tb.Fatal(err)
	}
	found := regexp.MustCompile("(?s)CREATE INDEX IF NOT EXISTS " + name + "\\b.*?;").Find(script)
	if found == nil {
		tb.Fatalf("the schema script has no index %s", name)
	}
	return string(found)
}

// historyWorld is a seeded store and the stamps the statements are asked about.
type historyWorld struct {
	s        *Store
	from, to string
	events   int
}

func newHistoryWorld(tb testing.TB, events int) *historyWorld {
	tb.Helper()
	s, err := Open(context.Background(), filepath.Join(tb.TempDir(), "relay.sqlite3"), "")
	if err != nil {
		tb.Fatal(err)
	}
	tb.Cleanup(func() { _ = s.Close() })
	for _, statement := range historySeed(events) {
		if _, err := s.DB.Exec(statement); err != nil {
			tb.Fatalf("%v\n%.200s", err, statement)
		}
	}
	window := float64(historyEpoch+events*historySpacing+60) - 1800
	return &historyWorld{s: s, from: SendStamp(window), to: SendStamp(window + 3600), events: events}
}

// answer is every row the statement returns, as text, in the order it returns them.
func (w *historyWorld) answer(tb testing.TB, query string, args []any) string {
	tb.Helper()
	rows, err := w.s.DB.Query(query, args...)
	if err != nil {
		tb.Fatal(err)
	}
	defer rows.Close()
	columns, _ := rows.Columns()
	var out []string
	for rows.Next() {
		values := make([]any, len(columns))
		pointers := make([]any, len(columns))
		for i := range values {
			pointers[i] = &values[i]
		}
		if err := rows.Scan(pointers...); err != nil {
			tb.Fatal(err)
		}
		out = append(out, fmt.Sprint(values...))
	}
	if err := rows.Err(); err != nil {
		tb.Fatal(err)
	}
	return strings.Join(out, "\n")
}

func (w *historyWorld) plan(tb testing.TB, query string, args []any) string {
	tb.Helper()
	rows, err := w.s.DB.Query("EXPLAIN QUERY PLAN "+query, args...)
	if err != nil {
		tb.Fatal(err)
	}
	defer rows.Close()
	var lines []string
	for rows.Next() {
		var id, parent, unused int
		var detail string
		if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
			tb.Fatal(err)
		}
		lines = append(lines, detail)
	}
	return strings.Join(lines, " | ")
}

// BenchmarkHistoryIndexes times each statement the indexes were added for with its indexes dropped (scan), with
// each of them built alone from the shipped script, and with all of them, on the same rows. Each case checks that
// every mode gives the same answer and that the plan names exactly the indexes the mode built, so a statement that
// stops using its index fails here instead of reading faster or slower by accident. The statements are the
// production ones (the constants the guard tests in their packages hold to the same plans) or their lookup shape.
func BenchmarkHistoryIndexes(b *testing.B) {
	w := newHistoryWorld(b, historyEvents())
	obligation := fmt.Sprintf("obl-%06d", w.events/16)
	relationship, event := fmt.Sprintf("rel-%04d", ((w.events/16)*8)%historyRelationships), fmt.Sprintf("ev-%06d", (w.events/16)*8)
	cases := []struct {
		name    string
		indexes []string
		query   string
		args    []any
	}{
		{"send_budget_hour", []string{"attempts_sent_at", "supervisor_attempts_transport_started"}, "SELECT * FROM " + RelationshipSpentSQL + " spent", []any{w.from, w.to, w.from, w.to}},
		{"pending_acks", []string{"acks_unverified"}, "SELECT a.event_id, a.ack_turn_id, a.ack_at, a.accepted, COALESCE(e.attempts, 0) AS attempts, e.last_reason, e.fingerprint, e.next_check_at FROM acks a LEFT JOIN ack_evidence e ON e.event_id = a.event_id LEFT JOIN deliveries d ON d.event_id = a.event_id WHERE a.verified = 'unverified_turn' AND (e.next_check_at IS NULL OR e.next_check_at <= ? OR (e.last_reason = ? AND d.state IN (?, ?))) ORDER BY COALESCE(e.next_check_at, 0), a.event_id LIMIT ?", []any{1e12, "delivery_unconfirmed", "dispatched", "inbox_only", 8}},
		{"verdict_write", []string{"sync_outbox_relationship_event"}, "SELECT sync_id,state,target,target_ref,external_ref,confirmed_at,last_error FROM sync_outbox WHERE relationship_id = ? AND event_id = ? AND subject_kind = 'verdict' AND target = 'coordination_document' ORDER BY rowid", []any{relationship, event}},
		{"obligation_message", []string{"supervisor_messages_obligation"}, "SELECT state,hold_reason,reading FROM supervisor_messages WHERE obligation_id=? ORDER BY staged_at DESC LIMIT 1", []any{obligation}},
		{"managed_start", []string{"managed_start_requests_dispatch"}, "SELECT child_task_id,relationship_id,execution_generation,workspace,marker_root,issue_key,standby_turn_id FROM managed_start_requests WHERE dispatch_request_id=?", []any{"dispatch-0007"}},
	}
	for _, c := range cases {
		modes := map[string][]string{"scan": nil}
		order := []string{"scan"}
		if len(c.indexes) > 1 {
			for _, name := range c.indexes {
				modes["only_"+name] = []string{name}
				order = append(order, "only_"+name)
			}
		}
		modes["index"] = c.indexes
		order = append(order, "index")
		var scanned string
		for _, mode := range order {
			built := map[string]bool{}
			for _, name := range modes[mode] {
				built[name] = true
			}
			for _, name := range c.indexes {
				statement := "DROP INDEX IF EXISTS " + name
				if built[name] {
					statement = historyIndexDDL(b, name)
				}
				if _, err := w.s.DB.Exec(statement); err != nil {
					b.Fatal(err)
				}
			}
			plan := w.plan(b, c.query, c.args)
			b.Logf("%s/%s plan: %s", c.name, mode, plan)
			for _, name := range c.indexes {
				if used := strings.Contains(plan, name); used != built[name] {
					b.Fatalf("%s/%s: index %s used=%v in %s", c.name, mode, name, used, plan)
				}
			}
			answer := w.answer(b, c.query, c.args)
			if mode == "scan" {
				scanned = answer
			} else if answer != scanned {
				b.Fatalf("%s/%s: the index changes the answer:\nscan:  %.300s\nmode:  %.300s", c.name, mode, scanned, answer)
			}
			b.Run(c.name+"/"+mode, func(b *testing.B) {
				for i := 0; i < b.N; i++ {
					w.answer(b, c.query, c.args)
				}
			})
		}
	}
}

// historyIndexNames are the indexes CRW-301 added, as the golden delta of the contract lists them.
var historyIndexNames = []string{"acks_unverified", "attempts_sent_at", "managed_start_requests_dispatch", "supervisor_attempts_transport_started", "supervisor_messages_obligation", "sync_outbox_relationship_event"}

// BenchmarkBuildHistoryIndex is the first build of each index on its own: the index is dropped (not timed) and
// created from the shipped script (timed). A build is one transaction that holds the write lock from its first
// page to its commit, so its time is the longest the lock is held for that index.
func BenchmarkBuildHistoryIndex(b *testing.B) {
	w := newHistoryWorld(b, historyEvents())
	for _, name := range historyIndexNames {
		ddl := historyIndexDDL(b, name)
		b.Run(name, func(b *testing.B) {
			for i := 0; i < b.N; i++ {
				b.StopTimer()
				if _, err := w.s.DB.Exec("DROP INDEX IF EXISTS " + name); err != nil {
					b.Fatal(err)
				}
				b.StartTimer()
				if _, err := w.s.DB.Exec(ddl); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// BenchmarkOpenBuildsHistoryIndexes is the open of a store, in three cases on the BenchmarkHistoryIndexes seed
// (CRW_HISTORY_EVENTS=800000 is the ten times larger store): built, an open of a store that has the indexes;
// first, the first open of a store that predates them (they are dropped before each round, untimed), whose
// extra time over built is the six builds; and first_with_writer, the same with a writer on another connection
// that tries a small write throughout and reports the longest wait of the writes that overlapped the open
// (max-writer-wait-ms). Each build is its own transaction (the script is not one), so a writer waits for the
// build in progress and not for the six together: the wait is of the order of the longest
// BenchmarkBuildHistoryIndex and the open is the sum.
func BenchmarkOpenBuildsHistoryIndexes(b *testing.B) {
	for _, c := range []struct {
		name         string
		drop, writer bool
	}{{"built", false, false}, {"first", true, false}, {"first_with_writer", true, true}} {
		b.Run(c.name, func(b *testing.B) {
			ctx := context.Background()
			b.StopTimer()
			w := newHistoryWorld(b, historyEvents())
			path := w.s.Path
			probe, err := sql.Open("sqlite", "file:"+path+"?_pragma=busy_timeout(30000)")
			if err != nil {
				b.Fatal(err)
			}
			probe.SetMaxOpenConns(1)
			defer probe.Close()
			type sample struct{ start, end time.Time }
			var longest time.Duration
			var overlapping int
			for round := 0; round < b.N; round++ {
				if c.drop {
					for _, name := range historyIndexNames {
						if _, err := w.s.DB.Exec("DROP INDEX IF EXISTS " + name); err != nil {
							b.Fatal(err)
						}
					}
				}
				if err := w.s.Close(); err != nil {
					b.Fatal(err)
				}
				var mu sync.Mutex
				var samples []sample
				stop, done := make(chan struct{}), make(chan struct{})
				if c.writer {
					go func() {
						defer close(done)
						for {
							select {
							case <-stop:
								return
							default:
							}
							start := time.Now()
							if _, err := probe.Exec("INSERT OR REPLACE INTO schema_meta (key, value) VALUES ('history_probe', 'x')"); err != nil {
								b.Error(err)
								return
							}
							mu.Lock()
							samples = append(samples, sample{start, time.Now()})
							mu.Unlock()
							time.Sleep(time.Millisecond)
						}
					}()
					time.Sleep(20 * time.Millisecond)
				} else {
					close(done)
				}
				openStart := time.Now()
				b.StartTimer()
				s, err := Open(ctx, path, "")
				b.StopTimer()
				openEnd := time.Now()
				close(stop)
				<-done
				if err != nil {
					b.Fatal(err)
				}
				w.s = s
				mu.Lock()
				for _, p := range samples {
					if p.end.After(openStart) && p.start.Before(openEnd) {
						overlapping++
						if wait := p.end.Sub(p.start); wait > longest {
							longest = wait
						}
					}
				}
				mu.Unlock()
			}
			if c.writer {
				if overlapping == 0 {
					b.Fatal("no probe write overlapped an open")
				}
				b.ReportMetric(float64(longest.Microseconds())/1000, "max-writer-wait-ms")
			}
			var present int
			if err := w.s.DB.QueryRow("SELECT COUNT(*) FROM sqlite_master WHERE type = 'index' AND name IN ('" + strings.Join(historyIndexNames, "','") + "')").Scan(&present); err != nil || present != len(historyIndexNames) {
				b.Fatalf("the open built %d of %d indexes: %v", present, len(historyIndexNames), err)
			}
		})
	}
}
