package delivery

import (
	"fmt"
	"math"
	"math/rand"
	"slices"
	"strings"
	"testing"
)

// CRW-270: what a parent's turn reads in a tick, and which parent starts the tick.
//
// The tests here use only what the scheduler already had, so they run against the code before the
// change: they measure the rows an opened walk holds (the code before the change read the parent's
// whole due list and kept the queues it chose, so this is not what was read, which the read
// counter of scheduler_cap_read_test.go proves) and the order in which parents are served.

// walkRows is how many due rows a parent's walk holds.
func walkRows(w *parentWalk) int {
	n := 0
	for _, q := range w.queues {
		n += len(q.rows)
	}
	return n
}

// newBacklogWorld is a parent whose four recipients (itself and its three children) each hold a run
// of perRecipient due deliveries: its own completions and a run of requests to each child. It
// returns each recipient's event ids in creation order.
func newBacklogWorld(t *testing.T, perRecipient int) (*scaleWorld, map[string][]string) {
	t.Helper()
	w := newScaleWorld(t, 3)
	byRecipient := map[string][]string{}
	for k := 0; k < perRecipient; k++ {
		byRecipient[scaleParent] = append(byRecipient[scaleParent], w.emit(k%3))
		w.f.clock.Advance(1)
		for i, r := range w.rels {
			event := w.create(i)
			w.queue(event, Revision, r.child)
			byRecipient[r.child] = append(byRecipient[r.child], event)
			w.f.clock.Advance(1)
		}
	}
	return w, byRecipient
}

// A parent's turn can attempt at most as many rows as it has attempts (the smaller of
// MaxSendsPerParentPerTick and the tick's budget), so it reads no more than that turn can
// use: the queue taken i-th (from 0) is reached only after i queues have spent an attempt each, and
// can use no more than the attempts that are left, which makes attempts*(attempts+1)/2 rows.
func TestCap_a_parents_turn_reads_no_more_rows_than_it_can_attempt(t *testing.T) {
	t.Parallel()
	for _, share := range []int{1, 2, 3} {
		t.Run(fmt.Sprintf("%d attempts", share), func(t *testing.T) {
			w, byRecipient := newBacklogWorld(t, 20)
			w.f.delivery.Policy.MaxSendsPerParentPerTick = share
			walk, err := w.sc.open(w.f.ctx, scaleParent, w.f.clock.Now(), share)
			mustDo(t, err)
			if walk == nil {
				t.Fatal("a parent with due deliveries opened no walk")
			}
			if got, most := walkRows(walk), share*(share+1)/2; got > most {
				t.Errorf("a turn of %d attempts read %d due rows, want at most %d", share, got, most)
			}
			if len(walk.queues) != share {
				t.Errorf("the turn took %d recipient queues, want %d", len(walk.queues), share)
			}
			// What it reads is the oldest of each recipient's rows, in creation order.
			for _, q := range walk.queues {
				for i, row := range q.rows {
					if want := byRecipient[q.recipient][i]; row.S("event_id") != want {
						t.Errorf("recipient %s: row %d is %s, want its %d-th oldest %s", q.recipient, i, row.S("event_id"), i+1, want)
					}
				}
			}
		})
	}
}

// rotation is several parents that each hold a run of due completions, served by a scheduler whose
// tick budget is one attempt: a tick serves exactly the parent the rotation starts it with.
type rotation struct {
	t    *testing.T
	f    *fixture
	sc   *Scheduler
	rids map[string]string
}

func newRotation(t *testing.T) *rotation {
	t.Helper()
	f := newFixture(t, "")
	f.clock.T = math.Floor(f.clock.T/3600) * 3600
	// The hourly send budget is not what these tests are about.
	f.delivery.Policy.MaxSendsPerRelationshipPerHour = 1000
	return &rotation{t: t, f: f, sc: &Scheduler{Delivery: f.delivery, Ack: NewAck(f.delivery), MaxSendsTick: 1}, rids: map[string]string{}}
}

// join registers the parent named name with events due completions.
func (r *rotation) join(name string, events int) {
	r.t.Helper()
	rid, _ := r.f.twoParentAssignment(name, events)
	r.rids[name] = rid
}

// leave takes every due row of the parent out of the due list, and back puts them in again.
func (r *rotation) leave(name string) {
	r.t.Helper()
	_, err := execSQL(r.f.ctx, r.f.store, "UPDATE deliveries SET hold_reason = ? WHERE relationship_id = ? AND state = ?", AttemptCap, r.rids[name], Queued)
	mustDo(r.t, err)
}

func (r *rotation) back(name string) {
	r.t.Helper()
	_, err := execSQL(r.f.ctx, r.f.store, "UPDATE deliveries SET hold_reason = NULL WHERE relationship_id = ? AND hold_reason = ?", r.rids[name], AttemptCap)
	mustDo(r.t, err)
}

// tick runs one delivery pass, then the daemon's sleep, and names the parent it sent to.
func (r *rotation) tick() string {
	r.t.Helper()
	before := len(r.f.host.sends)
	r.f.tick(r.sc)
	r.f.clock.Advance(scaleTick)
	sends := r.f.host.sends[before:]
	if len(sends) != 1 {
		r.t.Fatalf("a tick with a budget of one attempt made %d sends", len(sends))
	}
	return strings.TrimPrefix(sends[0].thread, "01parent-")
}

func (r *rotation) ticks(n int) []string {
	r.t.Helper()
	var served []string
	for i := 0; i < n; i++ {
		served = append(served, r.tick())
	}
	return served
}

func checkTurns(t *testing.T, got, want []string) {
	t.Helper()
	if !slices.Equal(got, want) {
		t.Errorf("parents were served in the order %v, want %v", got, want)
	}
}

func TestCap_parents_with_work_take_turns_in_id_order(t *testing.T) {
	t.Parallel()
	r := newRotation(t)
	for _, name := range []string{"a", "b", "c"} {
		r.join(name, 4)
	}
	checkTurns(t, r.ticks(6), []string{"a", "b", "c", "a", "b", "c"})
}

// A parent that arrives is served in its place in the order, and neither the parent served before
// it nor the one after it is served twice or skipped.
func TestCap_a_parent_that_joins_between_ticks_costs_no_one_a_turn(t *testing.T) {
	t.Parallel()
	r := newRotation(t)
	for _, name := range []string{"a", "b", "c"} {
		r.join(name, 4)
	}
	got := r.ticks(2)
	// "0" sorts before every other parent: by position it would push b into a second turn.
	r.join("0", 4)
	got = append(got, r.ticks(4)...)
	checkTurns(t, got, []string{"a", "b", "c", "0", "a", "b"})
}

// A parent that has nothing due any more is passed over, and the one after it takes its turn.
func TestCap_a_parent_that_leaves_between_ticks_costs_no_one_a_turn(t *testing.T) {
	t.Parallel()
	r := newRotation(t)
	for _, name := range []string{"a", "b", "c"} {
		r.join(name, 6)
	}
	got := r.ticks(1)
	r.leave("a")
	got = append(got, r.ticks(3)...)
	r.back("a")
	got = append(got, r.ticks(3)...)
	checkTurns(t, got, []string{"a", "b", "c", "b", "c", "a", "b"})
}

// Parents come and go at random: between two turns of a parent that stayed in the order the whole
// time, every other parent that stayed in it too is served exactly once. A parent that left and came
// back in between is not owed anything for the span it was away.
func TestCap_between_two_turns_of_a_parent_every_parent_that_stayed_is_served_once(t *testing.T) {
	t.Parallel()
	const ticks = 24
	names := []string{"0", "a", "b", "c", "d", "e"}
	rng := rand.New(rand.NewSource(270))
	r := newRotation(t)
	present := map[string]bool{}
	joined := map[string]bool{}
	for _, name := range []string{"a", "b", "c"} {
		r.join(name, ticks+2)
		joined[name], present[name] = true, true
	}
	var served []string
	var staying []map[string]bool
	for k := 0; k < ticks; k++ {
		if rng.Intn(5) < 2 {
			name := names[rng.Intn(len(names))]
			switch {
			case !joined[name]:
				r.join(name, ticks+2)
				joined[name], present[name] = true, true
			case present[name] && len(present) > 1:
				r.leave(name)
				delete(present, name)
			case !present[name]:
				r.back(name)
				present[name] = true
			}
		}
		now := map[string]bool{}
		for name := range present {
			now[name] = true
		}
		staying = append(staying, now)
		served = append(served, r.tick())
	}
	for i, x := range served {
		for j := i + 1; j < len(served); j++ {
			if served[j] != x {
				continue
			}
			// x was served at tick i and again at tick j. Unless x itself left and came back in between,
			// who stayed present for all of i..j?
			xStayed := true
			for k := i; k <= j; k++ {
				xStayed = xStayed && staying[k][x]
			}
			if !xStayed {
				break
			}
			for _, y := range names {
				stayed := true
				for k := i; k <= j; k++ {
					stayed = stayed && staying[k][y]
				}
				if !stayed || y == x {
					continue
				}
				turns := 0
				for k := i + 1; k < j; k++ {
					if served[k] == y {
						turns++
					}
				}
				if turns != 1 {
					t.Errorf("between the turns of %s at ticks %d and %d, %s that stayed was served %d times (order %v)", x, i, j, y, turns, served)
				}
			}
			break
		}
	}
}
