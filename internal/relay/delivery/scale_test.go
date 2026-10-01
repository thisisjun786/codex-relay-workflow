package delivery

import (
	"fmt"
	"math"
	"os"
	"path/filepath"
	"slices"
	"testing"

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
	}
	return w
}

// emit makes relationship i produce one more final completion event and queues its delivery.
func (w *scaleWorld) emit(i int) string {
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
	_, err = w.f.delivery.Enqueue(w.f.ctx, event, "", "")
	mustDo(w.t, err)
	w.created = append(w.created, event)
	return event
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
		refusal, err := w.f.delivery.SendRefusal(w.f.ctx, row.S("recipient_task_id"), w.f.clock.Now())
		mustDo(w.t, err)
		if refusal == HourlyCap {
			held++
		}
	}
	return held
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
		if got != int(w.f.delivery.Policy.MaxSendsPerRecipientPerHour) {
			t.Errorf("a runaway relationship got %d sends in its hour, want the cap %d", got, w.f.delivery.Policy.MaxSendsPerRecipientPerHour)
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
		if want := int(w.f.delivery.Policy.MaxSendsPerRecipientPerHour); runawaySends != want {
			t.Errorf("the runaway relationship got %d sends in its hour, want the cap %d", runawaySends, want)
		}
	})
}

// CRW-259 c3: deliveries that pile up while the parent is busy go out as soon as its turn ends,
// oldest first. A tick sends at most one message to a recipient (the minimum gap between two
// sends), so n deliveries take n ticks, and the first goes on the first tick.
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
	for k := 0; k < backlog; k++ {
		w.tick()
		if got := len(w.dispatched()); got != k+1 {
			t.Fatalf("after %d ticks of a free parent %d deliveries went out, want %d", k+1, got, k+1)
		}
	}
	if order := w.sentOrder(); !slices.Equal(order, w.created) {
		t.Errorf("sent in order %v, want creation order %v", order, w.created)
	}
}
