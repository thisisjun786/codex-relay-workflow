package delivery

import (
	"slices"
	"sort"
	"testing"
	"time"
)

// CRW-270: the bounded due reads and the parent rotation, after the change.

// allDue is a limit no due list reaches: the tests that read the whole due list as their oracle.
const allDue = 1 << 20

// reads counts what the due reads of a service return, by kind, and which parents were read.
type reads struct {
	rows, recipients int
	parents          []string
}

func (r *reads) watch(d *Service) {
	d.dueRead = func(kind, parent string, n int) {
		switch kind {
		case readRows:
			r.rows += n
			if !slices.Contains(r.parents, parent) {
				r.parents = append(r.parents, parent)
			}
		case readRecipients:
			r.recipients += n
		}
	}
}

// oracleAfterMarker is what the scheduler did before the read was bounded: the rows after the marker
// first, and the rest, wrapped, behind them.
func oracleAfterMarker(rows []Row, marker string) []Row {
	if marker == "" {
		return rows
	}
	split := sort.Search(len(rows), func(i int) bool { return rowKey(rows[i]) > marker })
	return append(slices.Clone(rows[split:]), rows[:split]...)
}

func ids(rows []Row) []string {
	var out []string
	for _, r := range rows {
		out = append(out, r.S("event_id"))
	}
	return out
}

func TestCapRead_EligibleRows_returns_the_oldest_limit_rows(t *testing.T) {
	t.Parallel()
	w := newScaleWorld(t, 2)
	var events []string
	for k := 0; k < 5; k++ {
		events = append(events, w.emit(k%2))
		w.f.clock.Advance(1)
	}
	now := w.f.clock.Now()
	for _, tc := range []struct {
		limit int
		want  []string
	}{{-1, nil}, {0, nil}, {1, events[:1]}, {3, events[:3]}, {5, events}, {allDue, events}} {
		rows, err := w.f.delivery.EligibleRows(w.f.ctx, scaleParent, now, tc.limit)
		mustDo(t, err)
		if got := ids(rows); !slices.Equal(got, tc.want) {
			t.Errorf("limit %d: got %v, want %v", tc.limit, got, tc.want)
		}
	}
}

// The window after a refusal marker is the old order cut at the limit: for every marker (none, the key
// of each row, a key no row holds any more, something that is not a key) and every limit, the rows are
// the first of what the scheduler used to take from the whole list.
func TestCapRead_dueRows_keeps_the_order_after_a_marker_for_every_marker_and_limit(t *testing.T) {
	t.Parallel()
	world, byRecipient := newBacklogWorld(t, 7)
	d := world.f.delivery
	now := world.f.clock.Now()
	recipient := world.rels[1].child
	full, err := d.dueRows(world.f.ctx, scaleParent, now, recipient, "", allDue)
	mustDo(t, err)
	if got := ids(full); !slices.Equal(got, byRecipient[recipient]) {
		t.Fatalf("the recipient's due rows are %v, want %v", got, byRecipient[recipient])
	}
	markers := []string{"", "7", "no|key", rowKey(Row{"first_seen_at": "0", "created_at": "0", "event_id": "0"})}
	for _, row := range full {
		markers = append(markers, rowKey(row))
	}
	for _, marker := range markers {
		want := oracleAfterMarker(full, marker)
		if _, _, _, isKey := splitKey(marker); !isKey {
			want = full
		}
		for limit := 1; limit <= len(full)+1; limit++ {
			got, err := d.dueRows(world.f.ctx, scaleParent, now, recipient, marker, limit)
			mustDo(t, err)
			if exp := ids(want[:min(limit, len(want))]); !slices.Equal(ids(got), exp) {
				t.Fatalf("marker %q limit %d: got %v, want %v", marker, limit, ids(got), exp)
			}
		}
	}
}

func TestCapRead_dueRecipients_rotates_after_the_recipient_last_attempted(t *testing.T) {
	t.Parallel()
	world, _ := newBacklogWorld(t, 2)
	d := world.f.delivery
	now := world.f.clock.Now()
	// The parent's recipients in id order: its three children, then itself.
	all := []string{world.rels[0].child, world.rels[1].child, world.rels[2].child, scaleParent}
	for _, tc := range []struct {
		after string
		limit int
		want  []string
	}{
		{"", 2, all[:2]},
		{"", 9, all},
		{all[1], 2, all[2:4]},
		{all[3], 2, all[:2]},
		{all[2], 3, []string{all[3], all[0], all[1]}},
		{"07", 1, all[:1]},
		{"", 0, nil},
		{"", -1, nil},
	} {
		got, err := d.dueRecipients(world.f.ctx, scaleParent, now, tc.after, tc.limit)
		mustDo(t, err)
		if !slices.Equal(got, tc.want) {
			t.Errorf("after %q limit %d: got %v, want %v", tc.after, tc.limit, got, tc.want)
		}
	}
}

// A tick reads the rows its turn can attempt and no more, however many are due: the cap is attempts
// * (attempts + 1) / 2 rows, and attempts is the smaller of the parent's share and the tick's budget.
func TestCapRead_a_tick_reads_no_more_than_its_turn_can_attempt(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name          string
		budget, share int
		most          int
	}{{"defaults", 0, 2, 3}, {"share 3", 0, 3, 6}, {"budget 1", 1, 2, 1}, {"share 1", 4, 1, 1}} {
		t.Run(tc.name, func(t *testing.T) {
			w, _ := newBacklogWorld(t, 20)
			w.sc.MaxSendsTick = tc.budget
			w.f.delivery.Policy.MaxSendsPerParentPerTick = tc.share
			var seen reads
			seen.watch(w.f.delivery)
			w.tick()
			// Every recipient holds more rows than the turn can use, so it reads exactly its cap.
			if seen.rows != tc.most {
				t.Errorf("the tick read %d due rows, want %d", seen.rows, tc.most)
			}
			attempts := tc.budget
			if attempts == 0 {
				attempts = 4
			}
			if want := min(tc.share, attempts); seen.recipients != want {
				t.Errorf("the tick read %d recipients, want %d", seen.recipients, want)
			}
		})
	}
}

// Only as many parents as the tick has attempts are opened: the first round gives each parent one
// attempt, so the parents after them cannot be reached in the tick.
func TestCapRead_a_tick_reads_only_as_many_parents_as_it_has_attempts(t *testing.T) {
	t.Parallel()
	r := newRotation(t)
	r.sc.MaxSendsTick = 2
	for _, name := range []string{"a", "b", "c", "d", "e"} {
		r.join(name, 4)
	}
	var seen reads
	seen.watch(r.f.delivery)
	before := len(r.f.host.sends)
	r.f.tick(r.sc)
	if want := []string{"01parent-a", "01parent-b"}; !slices.Equal(seen.parents, want) {
		t.Errorf("the tick read the parents %v, want %v", seen.parents, want)
	}
	if got := len(r.f.host.sends) - before; got != 2 {
		t.Errorf("the tick made %d sends, want 2", got)
	}
}

func TestCapRead_a_tick_with_no_attempts_reads_nothing_and_the_rotation_still_moves(t *testing.T) {
	t.Parallel()
	r := newRotation(t)
	r.sc.MaxSendsTick = -1
	for _, name := range []string{"a", "b", "c"} {
		r.join(name, 2)
	}
	var seen reads
	seen.watch(r.f.delivery)
	cursor := func() string {
		return r.f.one("SELECT cursor FROM discovery_cursors WHERE task_id = 'scheduler' AND listing = 'delivery_parents'").S("cursor")
	}
	r.f.tick(r.sc)
	if cursor() != "01parent-a" {
		t.Errorf("after the first tick the cursor is %q, want 01parent-a", cursor())
	}
	r.f.tick(r.sc)
	if cursor() != "01parent-b" {
		t.Errorf("after the second tick the cursor is %q, want 01parent-b", cursor())
	}
	if seen.rows != 0 || seen.recipients != 0 || len(r.f.host.sends) != 0 {
		t.Errorf("a tick with no attempts read %d rows and %d recipients and made %d sends", seen.rows, seen.recipients, len(r.f.host.sends))
	}
	r.sc.MaxSendsTick = 4
	r.sc.Delivery.Policy.MaxSendsPerParentPerTick = 0
	r.f.tick(r.sc)
	if seen.rows != 0 || seen.recipients != 0 {
		t.Errorf("a parent share of 0 read %d rows and %d recipients", seen.rows, seen.recipients)
	}
}

// A cursor an older scheduler wrote (an index) starts the first tick somewhere and is then replaced by
// the id of the parent that started it.
func TestCapRead_a_legacy_parent_cursor_is_replaced_by_a_parent_id(t *testing.T) {
	t.Parallel()
	r := newRotation(t)
	for _, name := range []string{"a", "b", "c"} {
		r.join(name, 3)
	}
	_, err := execSQL(r.f.ctx, r.f.store, "INSERT INTO discovery_cursors (task_id, listing, cursor, updated_at) VALUES ('scheduler', 'delivery_parents', '1', ?)", r.f.clock.ISO())
	mustDo(t, err)
	first := r.tick()
	id := r.f.one("SELECT cursor FROM discovery_cursors WHERE task_id = 'scheduler' AND listing = 'delivery_parents'").S("cursor")
	if id != "01parent-"+first {
		t.Fatalf("the cursor is %q after serving %s", id, first)
	}
	order := []string{"a", "b", "c"}
	next := order[(slices.Index(order, first)+1)%3]
	if got := r.tick(); got != next {
		t.Errorf("the tick after %s served %s, want %s", first, got, next)
	}
}

// A marker that is not a key (an index an older scheduler wrote) is no marker.
func TestCapRead_a_refusal_marker_that_is_not_a_key_is_no_marker(t *testing.T) {
	t.Parallel()
	w, byRecipient := newBacklogWorld(t, 5)
	recipient := w.rels[0].child
	w.exec("INSERT INTO discovery_cursors (task_id, listing, cursor, updated_at) VALUES ('scheduler', ?, ?, ?)", markerKey(scaleParent, recipient), "7", w.f.clock.ISO())
	walk, err := w.sc.open(w.f.ctx, scaleParent, w.f.clock.Now(), 1)
	mustDo(t, err)
	if walk == nil || len(walk.queues) != 1 || walk.queues[0].recipient != recipient {
		t.Fatalf("walk %+v", walk)
	}
	if got := ids(walk.queues[0].rows); !slices.Equal(got, byRecipient[recipient][:1]) {
		t.Errorf("the queue starts with %v, want the oldest row %v", got, byRecipient[recipient][:1])
	}
}

// A recipient whose rows were taken between the read of the recipients and the read of its rows has no queue.
func TestCapRead_a_recipient_without_rows_has_no_queue(t *testing.T) {
	w := &parentWalk{parent: "p", share: 2}
	w.add("gone", "", nil)
	w.add("here", "", []Row{{"event_id": "e"}})
	if len(w.queues) != 1 || w.queues[0].recipient != "here" {
		t.Errorf("queues %+v", w.queues)
	}
}

// The first parent of the rotation loses its due rows between the parent list and its walk (a
// relationship is paused, the deliver command sends a row): the next parent is served, and the
// tick after it serves the one after that, not the same parent twice.
func TestCapRead_a_first_parent_whose_rows_vanish_costs_no_one_a_second_turn(t *testing.T) {
	t.Parallel()
	r := newRotation(t)
	for _, name := range []string{"a", "b", "c"} {
		r.join(name, 6)
	}
	got := r.ticks(1)
	armed := true
	r.sc.afterParents = func() {
		if armed {
			armed = false
			r.leave("b")
		}
	}
	got = append(got, r.ticks(2)...)
	checkTurns(t, got, []string{"a", "c", "a"})
}

// CRW-270 measurement: one parent holds ten thousand due rows. The tick reads what its turn can
// attempt, and what it costs is logged (go test -v). The rows are synthetic: they are due and of the
// delivery direction the contract defines, but they are not events the relay produced, so an attempt
// on one is refused, which is the same cost before and after the change. The number of rows the tick
// reads is the measurement; the time is only bounded.
func TestCapRead_a_backlog_of_ten_thousand_rows_is_not_read_by_the_tick(t *testing.T) {
	const backlog = 10000
	w := newScaleWorld(t, 2)
	runaway := w.rels[0]
	stamp := w.f.clock.ISO()
	w.exec("WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i + 1 FROM n WHERE i < ?) INSERT INTO events (event_id, relationship_id, execution_generation, revision_hash, outcome, producer, turn_thread_id, turn_id, turn_status, receipt, first_seen_at, last_seen_at) SELECT 'bulk-' || printf('%05d', i), ?, 1, 'h', 'ready_for_review', 'child', ?, ?, 'completed', '{}', ?, ? FROM n", backlog, runaway.rid, runaway.child, runaway.turn, stamp, stamp)
	w.exec("WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i + 1 FROM n WHERE i < ?) INSERT INTO deliveries (event_id, relationship_id, kind, recipient_task_id, recipient_thread_id, state, attempt_count, created_at, updated_at) SELECT 'bulk-' || printf('%05d', i), ?, ?, ?, ?, 'queued', 0, ?, ? FROM n", backlog, runaway.rid, Completion, scaleParent, scaleParent, stamp, stamp)
	now := w.f.clock.Now()
	var seen reads
	seen.watch(w.f.delivery)
	began := time.Now()
	mustDo(t, w.sc.Deliver(w.f.ctx, w.f.host, now, &TickCounts{}))
	elapsed := time.Since(began)
	t.Logf("CRW-270 measurement: %d due rows of one parent; one Deliver tick read %d rows and %d recipients and took %v", backlog, seen.rows, seen.recipients, elapsed)
	if most := 3; seen.rows > most {
		t.Errorf("the tick read %d of %d due rows, want at most %d", seen.rows, backlog, most)
	}
	if elapsed > 3*time.Second {
		t.Errorf("a tick over %d due rows took %v", backlog, elapsed)
	}
}
