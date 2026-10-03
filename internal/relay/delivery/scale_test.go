package delivery

import (
	"fmt"
	"math"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// CRW-259: one parent receives deliveries from many children.
//
// The world is the daemon's delivery pass over a fake host: every tick is the 20 seconds the
// daemon sleeps between passes (daemon.DefaultPolicy PollInterval), and a send does not make the
// fake parent busy, so these tests measure the limits and the scheduler, not the host.

const (
	scaleParent = "01parent-scale"
	// scaleTick is the daemon's poll interval in seconds.
	scaleTick = 20.0
)

type scaleRel struct {
	rid, child, turn, root string
	emitted                int
}

type scaleWorld struct {
	t    *testing.T
	f    *fixture
	sc   *Scheduler
	rels []*scaleRel
	// created lists event ids in the order the events were created.
	created []string
}

// newScaleWorld registers one parent with n active relationships (one child each) and starts the
// clock at the opening of an hour window, so the hour the tests simulate is one rate window.
func newScaleWorld(t *testing.T, n int) *scaleWorld {
	t.Helper()
	f := newFixture(t, "")
	f.clock.T = math.Floor(f.clock.T/3600) * 3600
	f.host.addThread(scaleParent)
	w := &scaleWorld{t: t, f: f, sc: &Scheduler{Delivery: f.delivery, Ack: NewAck(f.delivery)}}
	for k := 0; k < n; k++ {
		child := fmt.Sprintf("01child-scale-%02d", k)
		turn := fmt.Sprintf("turn-scale-%02d", k)
		root := filepath.Join(f.root, fmt.Sprintf("child-%02d", k))
		mustDo(t, os.MkdirAll(root, 0o755))
		f.host.addThread(child)
		rid := f.register(regOpts{issue: fmt.Sprintf("SCALE-%d", k+1), dispatchRequest: fmt.Sprintf("dispatch-scale-%d", k), parent: scaleParent,
			parentCwd: "/repo-scale", child: child, childRoot: root, turn: turn, scopeRef: fmt.Sprintf("linear://scale-%d", k),
			recipients: []string{scaleParent, child}, parentOnlySettings: k == 0})
		w.rels = append(w.rels, &scaleRel{rid: rid, child: child, turn: turn, root: root})
		// A request to the child is sent with the child's own authorized settings.
		w.exec("INSERT INTO authorized_settings (task_id, settings, source, recorded_at) VALUES (?,?,?,?)", child, taskSettings(root), "creation_result", f.clock.ISO())
	}
	return w
}

// create makes relationship i produce one more final event without queueing its delivery.
func (w *scaleWorld) create(i int) string {
	w.t.Helper()
	r := w.rels[i]
	r.emitted++
	path := filepath.Join(r.root, fmt.Sprintf("out-%d.txt", r.emitted))
	mustDo(w.t, os.WriteFile(path, []byte(fmt.Sprintf("%s-%d", r.child, r.emitted)), 0o644))
	entries, err := store.BuildManifest([]string{path}, []string{r.root})
	mustDo(w.t, err)
	revision, err := store.ManifestRevision(entries)
	mustDo(w.t, err)
	attempt := r.emitted
	event, err := store.EventID(r.rid, 1, revision, "ready_for_review", r.turn, &attempt)
	mustDo(w.t, err)
	payload := Obj{{Key: "eventId", Value: event}, {Key: "relationshipId", Value: r.rid}, {Key: "executionGeneration", Value: int64(1)}, {Key: "attempt", Value: int64(attempt)}, {Key: "revisionHash", Value: revision}, {Key: "outcome", Value: "ready_for_review"}, {Key: "producer", Value: "child"},
		{Key: "turnRef", Value: Obj{{Key: "threadId", Value: r.child}, {Key: "turnId", Value: r.turn}, {Key: "turnStatus", Value: "completed"}}},
		{Key: "manifest", Value: []any{Obj{{Key: "path", Value: path}, {Key: "sha256", Value: entries[0].SHA256}, {Key: "bytes", Value: *entries[0].Bytes}}}}, {Key: "emittedAt", Value: w.f.clock.ISO()}}
	_, err = w.f.accept(payload, store.AcceptOptions{})
	mustDo(w.t, err)
	w.created = append(w.created, event)
	return event
}

// queue queues the delivery of a created event: the completion to the parent, or with kind
// Revision a request to the child.
func (w *scaleWorld) queue(event, kind, recipient string) {
	w.t.Helper()
	_, err := w.f.delivery.Enqueue(w.f.ctx, event, kind, recipient)
	mustDo(w.t, err)
}

// emit makes relationship i produce one more final completion event and queues its delivery.
func (w *scaleWorld) emit(i int) string {
	w.t.Helper()
	event := w.create(i)
	w.queue(event, "", "")
	return event
}

func (w *scaleWorld) exec(query string, args ...any) {
	w.t.Helper()
	_, err := execSQL(w.f.ctx, w.f.store, query, args...)
	mustDo(w.t, err)
}

// tick is one delivery pass at the current time, then the daemon's sleep.
func (w *scaleWorld) tick() {
	w.t.Helper()
	mustDo(w.t, w.sc.Deliver(w.f.ctx, w.f.host, w.f.clock.Now(), &TickCounts{}))
	w.f.clock.Advance(scaleTick)
}

func (w *scaleWorld) dispatched() []string {
	var out []string
	for _, e := range w.created {
		if w.f.row(e).S("state") == Dispatched {
			out = append(out, e)
		}
	}
	return out
}

// capHolds counts the created deliveries still waiting that the hourly cap is holding right now.
func (w *scaleWorld) capHolds() int {
	w.t.Helper()
	held := 0
	for _, e := range w.created {
		row := w.f.row(e)
		if row.S("state") == Dispatched || row.S("state") == Superseded {
			continue
		}
		refusal, err := w.f.delivery.SendRefusal(w.f.ctx, row.S("relationship_id"), row.S("recipient_task_id"), w.f.clock.Now())
		mustDo(w.t, err)
		if refusal == HourlyCap {
			held++
		}
	}
	return held
}

// sentTo is the events the host was sent for one recipient, in send order.
func (w *scaleWorld) sentTo(thread string) []string {
	w.t.Helper()
	var out []string
	for _, s := range w.f.host.sends {
		if s.thread == thread {
			out = append(out, w.f.one("SELECT event_id FROM attempts WHERE request_id = ?", s.requestID).S("event_id"))
		}
	}
	return out
}

// sentOrder is the events the host was sent, in send order.
func (w *scaleWorld) sentOrder() []string {
	w.t.Helper()
	var out []string
	for _, s := range w.f.host.sends {
		out = append(out, w.f.one("SELECT event_id FROM attempts WHERE request_id = ?", s.requestID).S("event_id"))
	}
	return out
}

// CRW-259 c1: twenty children each produce three events within one hour, sixty deliveries to
// one parent. Before the redesign the parent's twelve sends an hour held every later one.
func TestScale_one_parent_receives_every_delivery_of_twenty_children(t *testing.T) {
	const children, perChild = 20, 3
	w := newScaleWorld(t, children)
	holds := 0
	emitted := 0
	// One event every 40 seconds for 40 minutes, then the rest of the hour to deliver them.
	for tick := 0; tick < 175; tick++ {
		if tick%2 == 0 && emitted < children*perChild {
			w.emit(emitted % children)
			emitted++
		}
		w.tick()
		holds += w.capHolds()
	}
	if got := len(w.dispatched()); got != children*perChild {
		t.Errorf("delivered %d of %d deliveries within the hour", got, children*perChild)
	}
	if holds != 0 {
		t.Errorf("the hourly cap held a delivery on %d tick readings", holds)
	}
}

// CRW-259 c2: the cap stays a fence. One relationship that produces far more deliveries than any
// real child does in a short time is limited, with or without other relationships beside it.
func TestScale_a_runaway_relationship_is_still_limited(t *testing.T) {
	const runaway = 60
	t.Run("alone", func(t *testing.T) {
		w := newScaleWorld(t, 1)
		for k := 0; k < runaway; k++ {
			w.emit(0)
			w.tick()
		}
		got := len(w.dispatched())
		if got != int(w.f.delivery.Policy.MaxSendsPerRelationshipPerHour) {
			t.Errorf("a runaway relationship got %d sends in its hour, want the cap %d", got, w.f.delivery.Policy.MaxSendsPerRelationshipPerHour)
		}
		if w.capHolds() == 0 {
			t.Error("nothing is waiting on the cap after the runaway")
		}
	})
	t.Run("among other children", func(t *testing.T) {
		const siblings = 19
		w := newScaleWorld(t, siblings+1)
		var calm []string
		for k := 0; k < runaway; k++ {
			w.emit(0)
			if k%3 == 0 && k/3 < siblings {
				calm = append(calm, w.emit(1+k/3))
			}
			w.tick()
		}
		for k := 0; k < 20; k++ {
			w.tick()
		}
		for _, e := range calm {
			if w.f.row(e).S("state") != Dispatched {
				t.Fatalf("a calm child's delivery %s is %s: the runaway took the parent's budget", e, w.f.row(e).S("state"))
			}
		}
		runawaySends := 0
		for _, e := range w.created {
			if w.f.one("SELECT relationship_id FROM events WHERE event_id = ?", e).S("relationship_id") == w.rels[0].rid && w.f.row(e).S("state") == Dispatched {
				runawaySends++
			}
		}
		if want := int(w.f.delivery.Policy.MaxSendsPerRelationshipPerHour); runawaySends != want {
			t.Errorf("the runaway relationship got %d sends in its hour, want the cap %d", runawaySends, want)
		}
	})
}

// CRW-259 c3: deliveries that pile up while the parent is busy go out once its turn ends, oldest first.
// A tick sends at most one message to a recipient (the minimum gap between two sends), so n deliveries
// take n ticks. CRW-261: the busy backoff grows with each busy answer, and only the oldest delivery
// meets the busy parent while it waits (the ones behind it are not attempted), so the first goes at the
// first tick on or after the end of the oldest delivery's backoff, which is at most BusyMax away; the
// others follow one a tick, in creation order.
func TestScale_a_busy_backlog_drains_in_creation_order(t *testing.T) {
	const backlog = 10
	w := newScaleWorld(t, backlog)
	w.f.host.threads[scaleParent].status = "active"
	for i := 0; i < backlog; i++ {
		w.emit(i)
		w.f.clock.Advance(1)
	}
	// A long turn: many ticks pass, and the scheduler keeps asking whether the parent is free.
	for k := 0; k < 7; k++ {
		w.tick()
	}
	if sent := len(w.f.host.sends); sent != 0 {
		t.Fatalf("%d messages went to a busy parent", sent)
	}
	w.f.host.threads[scaleParent].status = "idle"
	waited := 0
	for len(w.dispatched()) == 0 {
		if waited++; float64(waited)*scaleTick > w.f.delivery.Policy.BusyMax+scaleTick {
			t.Fatalf("a free parent was sent nothing in %.0f s, longer than the busy backoff's ceiling %.0f s", float64(waited)*scaleTick, w.f.delivery.Policy.BusyMax)
		}
		w.tick()
	}
	for k := 1; k < backlog; k++ {
		w.tick()
		if got := len(w.dispatched()); got != k+1 {
			t.Fatalf("%d ticks after the first delivery went out %d deliveries had gone, want %d", k, got, k+1)
		}
	}
	if order := w.sentOrder(); !slices.Equal(order, w.created) {
		t.Errorf("sent in order %v, want creation order %v", order, w.created)
	}
}

// A delivery goes out in the order its event was created, not the order it was queued in: a
// delivery refused at enqueue and queued again later keeps its place.
func TestScale_deliveries_keep_event_creation_order_not_queueing_order(t *testing.T) {
	w := newScaleWorld(t, 3)
	var events []string
	for i := 0; i < 3; i++ {
		events = append(events, w.create(i))
		w.f.clock.Advance(1)
	}
	for _, i := range []int{2, 0, 1} {
		w.queue(events[i], "", "")
		w.f.clock.Advance(1)
	}
	for k := 0; k < 3; k++ {
		w.tick()
	}
	if order := w.sentOrder(); !slices.Equal(order, events) {
		t.Errorf("sent in order %v, want event creation order %v", order, events)
	}
}

// A request to a child is a different recipient from the parent's backlog: a busy parent does not
// hold it back, and the parent's own backlog drains once the parent is free.
func TestScale_a_busy_parent_does_not_hold_back_a_request_to_a_child(t *testing.T) {
	w := newScaleWorld(t, 4)
	w.f.host.threads[scaleParent].status = "active"
	var backlog []string
	for i := 0; i < 3; i++ {
		backlog = append(backlog, w.emit(i))
		w.f.clock.Advance(1)
	}
	request := w.create(3)
	w.queue(request, Revision, w.rels[3].child)
	w.tick()
	if got := w.sentTo(w.rels[3].child); !slices.Equal(got, []string{request}) {
		t.Fatalf("the child was sent %v on the first tick, want its request %s", got, request)
	}
	if len(w.sentTo(scaleParent)) != 0 {
		t.Fatal("a message went to the busy parent")
	}
	w.f.host.threads[scaleParent].status = "idle"
	for k := 0; k < len(backlog); k++ {
		w.tick()
	}
	if got := w.sentTo(scaleParent); !slices.Equal(got, backlog) {
		t.Errorf("the parent was sent %v, want its backlog in creation order %v", got, backlog)
	}
}

// Recipients that cannot take a send (busy children with requests queued) cost an attempt each
// and no more: the idle parent behind them is reached within ceil(recipients / share) ticks, and
// keeps being reached.
func TestScale_busy_recipients_do_not_keep_an_idle_one_waiting(t *testing.T) {
	w := newScaleWorld(t, 6)
	for i := 0; i < 5; i++ {
		w.f.host.threads[w.rels[i].child].status = "active"
		w.queue(w.create(i), Revision, w.rels[i].child)
		w.f.clock.Advance(1)
	}
	var completions []string
	for k := 0; k < 3; k++ {
		completions = append(completions, w.emit(5))
		w.f.clock.Advance(1)
	}
	// Six recipients under one parent, two attempts a tick: the parent is reached by the third tick.
	for k := 0; k < 3; k++ {
		w.tick()
	}
	if len(w.sentTo(scaleParent)) == 0 {
		t.Fatal("the idle parent was not sent anything in three ticks behind five busy children")
	}
	for k := 0; k < 12; k++ {
		w.tick()
	}
	if got := w.sentTo(scaleParent); !slices.Equal(got, completions[:len(got)]) || len(got) != len(completions) {
		t.Errorf("the parent was sent %v, want its completions %v in creation order", got, completions)
	}
}

// A delivery that is refused before it is claimed (here its relationship no longer lists the
// parent as a recipient) keeps its state and holds nothing: the deliveries behind it, to the same
// recipient, go out within ceil((refused + 1) / attempts) ticks, in creation order.
func TestScale_a_refused_delivery_does_not_hold_the_ones_behind_it(t *testing.T) {
	const refused, good = 3, 4
	w := newScaleWorld(t, refused+good)
	var behind []string
	for i := 0; i < refused+good; i++ {
		e := w.emit(i)
		if i >= refused {
			behind = append(behind, e)
		}
		w.f.clock.Advance(1)
	}
	for i := 0; i < refused; i++ {
		w.exec("UPDATE relationships SET allowed_recipients = '[]' WHERE relationship_id = ?", w.rels[i].rid)
	}
	// attempts a tick for the parent: 2 (MaxSendsPerParentPerTick); the first good row is the
	// fourth in the walk, so it goes by the second tick.
	for k := 0; k < 2; k++ {
		w.tick()
	}
	if got := w.sentTo(scaleParent); len(got) == 0 || got[0] != behind[0] {
		t.Fatalf("after two ticks the parent was sent %v, want %s first", got, behind[0])
	}
	for k := 0; k < 4*(refused+1); k++ {
		w.tick()
	}
	if got := w.sentTo(scaleParent); !slices.Equal(got, behind) {
		t.Errorf("the parent was sent %v, want the deliveries behind the refused ones, in creation order %v", got, behind)
	}
	for i := 0; i < refused; i++ {
		row := w.f.row(w.created[i])
		if row.S("state") != Queued || !row.N("hold_reason") {
			t.Errorf("a refused delivery was moved: state %s hold %v", row.S("state"), row.Opt("hold_reason"))
		}
	}
}

// A relationship that has spent its hour is not due until the window reopens: its queued rows
// spend no attempt, and the calm sibling queued after them goes out on the first tick.
func TestScale_a_relationship_at_its_cap_is_not_attempted_at_all(t *testing.T) {
	w := newScaleWorld(t, 2)
	w.f.spendHour(w.rels[0].rid, scaleParent, int(w.f.delivery.Policy.MaxSendsPerRelationshipPerHour), w.f.clock.Now())
	var capped []string
	for k := 0; k < 8; k++ {
		capped = append(capped, w.emit(0))
		w.f.clock.Advance(1)
	}
	calm := w.emit(1)
	w.tick()
	if got := w.sentTo(scaleParent); !slices.Equal(got, []string{calm}) {
		t.Fatalf("the first tick sent %v, want only the calm sibling's %s", got, calm)
	}
	for _, e := range capped {
		if n := w.f.count("SELECT COUNT(*) AS c FROM attempts WHERE event_id = ?", e); n != 0 {
			t.Fatalf("a delivery of the capped relationship was attempted %d times", n)
		}
	}
	// The window reopens: they are due again and go out one a tick, in creation order.
	w.f.clock.T = math.Floor(w.f.clock.Now()/3600)*3600 + 3600
	for k := 0; k < len(capped); k++ {
		w.tick()
	}
	if got := w.sentTo(scaleParent)[1:]; !slices.Equal(got, capped) {
		t.Errorf("after the window reopened the parent was sent %v, want %v", got, capped)
	}
}

// A cursor value an older scheduler wrote (an index) is read as no pointer.
func TestScale_a_legacy_scheduler_cursor_is_not_a_pointer(t *testing.T) {
	w := newScaleWorld(t, 3)
	for _, c := range [][2]string{{"deliver:" + scaleParent, "7"}, {"delivery_parents", "3"}} {
		w.exec("INSERT INTO discovery_cursors (task_id, listing, cursor, updated_at) VALUES ('scheduler', ?, ?, ?)", c[0], c[1], w.f.clock.ISO())
	}
	var events []string
	for i := 0; i < 3; i++ {
		events = append(events, w.emit(i))
		w.f.clock.Advance(1)
	}
	for k := 0; k < 3; k++ {
		w.tick()
	}
	if order := w.sentOrder(); !slices.Equal(order, events) {
		t.Errorf("sent in order %v, want creation order %v", order, events)
	}
	if got := w.f.one("SELECT cursor FROM discovery_cursors WHERE task_id = 'scheduler' AND listing = ?", "deliver:"+scaleParent).S("cursor"); got != scaleParent {
		t.Errorf("the parent's pointer is %q, want the recipient last attempted %q", got, scaleParent)
	}
}

// Whether a relationship has spent its hour is one grouped read joined to the due list, not an
// expression evaluated for each candidate: ten thousand queued deliveries of a runaway relationship
// are set aside in a fraction of a second (the per-candidate form took six seconds).
func TestScale_a_large_backlog_of_a_capped_relationship_is_set_aside_in_linear_time(t *testing.T) {
	const backlog = 10000
	w := newScaleWorld(t, 2)
	runaway := w.rels[0]
	w.f.spendHour(runaway.rid, scaleParent, int(w.f.delivery.Policy.MaxSendsPerRelationshipPerHour), w.f.clock.Now())
	stamp := w.f.clock.ISO()
	w.exec("WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i + 1 FROM n WHERE i < ?) INSERT INTO events (event_id, relationship_id, execution_generation, revision_hash, outcome, producer, turn_thread_id, turn_id, turn_status, receipt, first_seen_at, last_seen_at) SELECT 'bulk-' || i, ?, 1, 'h', 'ready_for_review', 'child', ?, ?, 'completed', '{}', ?, ? FROM n", backlog, runaway.rid, runaway.child, runaway.turn, stamp, stamp)
	w.exec("WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i + 1 FROM n WHERE i < ?) INSERT INTO deliveries (event_id, relationship_id, kind, recipient_task_id, recipient_thread_id, state, attempt_count, created_at, updated_at) SELECT 'bulk-' || i, ?, 'completion', ?, ?, 'queued', 0, ?, ? FROM n", backlog, runaway.rid, scaleParent, scaleParent, stamp, stamp)
	calm := w.emit(1)
	began := time.Now()
	parents, err := w.f.delivery.EligibleParents(w.f.ctx, w.f.clock.Now())
	mustDo(t, err)
	rows, err := w.f.delivery.EligibleRows(w.f.ctx, scaleParent, w.f.clock.Now(), allDue)
	mustDo(t, err)
	if elapsed := time.Since(began); elapsed > 3*time.Second {
		t.Errorf("selecting among %d queued deliveries took %v", backlog, elapsed)
	}
	if !slices.Equal(parents, []string{scaleParent}) || len(rows) != 1 || rows[0].S("event_id") != calm {
		t.Errorf("due: parents %v, %d rows; want the parent and only the calm sibling's %s", parents, len(rows), calm)
	}
}

// A negative limit lists nothing; it does not slice a list with a negative bound.
func TestScale_a_negative_limit_lists_nothing(t *testing.T) {
	w := newScaleWorld(t, 2)
	w.emit(0)
	w.emit(1)
	for _, limit := range []int{-1, 0} {
		rows, err := w.f.delivery.Eligible(w.f.ctx, w.f.clock.Now(), limit, limit, 0)
		if err != nil || len(rows) != 0 {
			t.Errorf("limit %d: %d rows, %v", limit, len(rows), err)
		}
	}
	if rows, err := w.f.delivery.Eligible(w.f.ctx, w.f.clock.Now(), 4, -1, 0); err != nil || len(rows) != 0 {
		t.Errorf("a negative share: %d rows, %v", len(rows), err)
	}
}

// An attempt that woke nobody (it failed before the send) is not a send: fourteen failed claims of one
// relationship leave its hour untouched, and the next claim goes out.
func TestScale_attempts_that_failed_before_the_send_do_not_spend_the_hour(t *testing.T) {
	w := newScaleWorld(t, 1)
	d := w.f.delivery
	d.Policy.PresendBase, d.Policy.PresendMax, d.Policy.MaxAttempts = 0, 0, 100
	event := w.emit(0)
	rel := w.rels[0].rid
	window := math.Floor(w.f.clock.Now()/3600) * 3600
	failures := int(d.Policy.MaxSendsPerRelationshipPerHour) + 2
	for i := 0; i < failures; i++ {
		w.f.host.script = []string{"read_fail"}
		w.f.clock.Advance(6)
		if record := w.f.mustAttempt(event, at(w.f.clock.Now())); record != nil {
			if state := pyjson.Text(record.Get("deliveryState")); state != WithheldPreSend {
				t.Fatalf("failure %d settled as %s, want a failure before the send", i+1, state)
			}
		}
	}
	if n := w.f.count("SELECT COUNT(*) AS c FROM attempts WHERE event_id = ?", event); n != int64(failures) {
		t.Fatalf("%d claims were made, want %d", n, failures)
	}
	spent, err := w.f.store.RelationshipSends(w.f.ctx, rel, scaleParent, window)
	mustDo(t, err)
	if spent != 0 {
		t.Fatalf("%d failed claims spent %d of the relationship's hour, want 0", failures, spent)
	}
	w.f.clock.Advance(6)
	record := w.f.mustAttempt(event, at(w.f.clock.Now()))
	if record == nil || pyjson.Text(record.Get("deliveryState")) != Dispatched {
		t.Fatalf("the claim after the failures was not sent: %v", record)
	}
	if spent, err = w.f.store.RelationshipSends(w.f.ctx, rel, scaleParent, window); err != nil || spent != 1 {
		t.Fatalf("the send spent %d, %v; want 1", spent, err)
	}
}
