package daemon

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/delivery"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// Throughput of the observation pass (CRW-258): what it reads, in which order, and what it leaves alone.
// The relationships here are named by index: relationship rel-NN has parent parent-NN, child child-NN and
// anchor anchor-NN, so a host read of anchor-NN is a read on behalf of rel-NN.

func name(prefix string, i int) string { return fmt.Sprintf("%s-%02d", prefix, i) }

func throughputStore(t *testing.T) (context.Context, *store.Store) {
	t.Helper()
	ctx := context.Background()
	s, err := fixtureStore(ctx, filepath.Join(t.TempDir(), "relay.sqlite3"), "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return ctx, s
}

// seedIndexed seeds relationship rel-NN anchored at anchor-NN.
func seedIndexed(t *testing.T, s *store.Store, i int) {
	t.Helper()
	seed(t, s, name("rel", i), name("parent", i), name("child", i), name("anchor", i))
}

func exec(t *testing.T, s *store.Store, statement string, args ...any) {
	t.Helper()
	if _, err := s.DB.Exec(statement, args...); err != nil {
		t.Fatalf("%s: %v", statement, err)
	}
}

func count(t *testing.T, s *store.Store, query string, args ...any) (n int) {
	t.Helper()
	if err := s.DB.QueryRow(query, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// settleTurn records the relay's settlement of a turn for relationship i.
func settleTurn(t *testing.T, s *store.Store, i int, turn string) {
	t.Helper()
	exec(t, s, "INSERT INTO assignment_settlements(relationship_id,thread_id,turn_id,terminal_status,settled_at) VALUES(?,?,?,'completed','2023-11-14T22:13:20Z')", name("rel", i), name("child", i), turn)
}

// stageClaim stores a claim relationship i's child emitted from inside the turn.
func stageClaim(t *testing.T, s *store.Store, i int, turn string) {
	t.Helper()
	exec(t, s, "INSERT INTO events(event_id,relationship_id,execution_generation,revision_hash,outcome,producer,turn_thread_id,turn_id,turn_status,receipt,stage,staged_at,first_seen_at,last_seen_at) VALUES(?,?,1,'rev','ready_for_review','child',?,?,'inProgress','{}','staged','2023-11-14T22:13:20Z','2023-11-14T22:13:20Z','2023-11-14T22:13:20Z')",
		"staged-"+name("rel", i)+"-"+turn, name("rel", i), name("child", i), turn)
}

// admitTurn admits a turn to generation gen of relationship i, as the store's identity check accepts it.
func admitTurn(t *testing.T, s *store.Store, i, gen int, anchor, turn string) {
	t.Helper()
	admitTurnAt(t, s, i, gen, anchor, turn, "2023-11-14T22:13:20Z")
}

func admitTurnAt(t *testing.T, s *store.Store, i, gen int, anchor, turn, at string) {
	t.Helper()
	exec(t, s, "INSERT INTO generation_turns(relationship_id,execution_generation,turn_id,evidence,actor,detail,admitted_at) VALUES(?,?,?,?,'child','admitted',?)", name("rel", i), gen, turn, "explicit_admission_bound:"+anchor, at)
}

// movingClock is a clock whose every stamp is later than the last, as a real one is between two reads. It starts
// a day after the rows the helpers seed, which are stamped 2023-11-14T22:13:20Z.
func movingClock() *delivery.FakeClock {
	c := &delivery.FakeClock{T: 1700000000 + 86400}
	c.OnISO = func() { c.T += 0.001 }
	return c
}

// indexOf is the relationship index a host read of turn was made for, or -1.
func indexOf(turn string) int {
	var i int
	if _, err := fmt.Sscanf(turn, "anchor-%d", &i); err != nil {
		return -1
	}
	return i
}

// perTick runs ticks ticks and returns, for each, the relationship indexes the host was read for.
func perTick(t *testing.T, ctx context.Context, d *Daemon, host *observationHost, ticks int) [][]int {
	t.Helper()
	var out [][]int
	for range ticks {
		before := len(host.reads)
		if _, err := d.Tick(ctx); err != nil {
			t.Fatal(err)
		}
		served := []int{}
		for _, turn := range host.reads[before:] {
			served = append(served, indexOf(turn))
		}
		out = append(out, served)
	}
	return out
}

// withinTicks fails unless every one of the relationships 0..total-1 that is meant to be served is served in
// every window of n consecutive ticks.
func withinTicks(t *testing.T, label string, reads [][]int, members []int, n int) {
	t.Helper()
	for start := 0; start+n <= len(reads); start++ {
		seen := map[int]bool{}
		for _, tick := range reads[start : start+n] {
			for _, i := range tick {
				seen[i] = true
			}
		}
		for _, i := range members {
			if !seen[i] {
				t.Errorf("%s: relationship %d was not read in ticks %d..%d (per tick: %v)", label, i, start, start+n-1, reads)
				return
			}
		}
	}
}

// longestWaitingFirst fails when a tick serves a relationship while another one that was last read earlier
// (or never) is left out. members are the relationships competing for the budget.
func longestWaitingFirst(t *testing.T, reads [][]int, members []int) {
	t.Helper()
	last := map[int]int{}
	for _, i := range members {
		last[i] = -1
	}
	for k, tick := range reads {
		served := map[int]bool{}
		newest := -2
		for _, i := range tick {
			if _, ok := last[i]; ok {
				served[i] = true
				newest = max(newest, last[i])
			}
		}
		for _, i := range members {
			if !served[i] && last[i] < newest && len(served) > 0 {
				t.Errorf("tick %d served a relationship last read at tick %d while %d, last read at tick %d, waited longer (per tick: %v)", k, newest, i, last[i], reads)
				return
			}
		}
		for i := range served {
			last[i] = k
		}
	}
}

// c1: of 50 active relationships 45 have nothing to do and 5 have staged receipts whose turns the host
// reports completed. One tick finalizes all five and reads nothing else.
func TestStagedReceiptsAmongManyRelationshipsAreFinalizedInOneTick(t *testing.T) {
	ctx, s := throughputStore(t)
	staged := []int{3, 14, 26, 38, 47}
	for i := range 50 {
		seedIndexed(t, s, i)
		if slices.Contains(staged, i) {
			stageClaim(t, s, i, name("anchor", i))
		} else {
			settleTurn(t, s, i, name("anchor", i))
		}
	}
	host := &observationHost{status: "completed"}
	d := New(s, host, &delivery.FakeClock{T: 1700000000}, nil)
	d.Policy.MaxSends = -1 // settlement, not delivery
	finalized := func() int { return count(t, s, "SELECT COUNT(*) FROM events WHERE stage='final'") }
	ticks := 0
	var first Report
	for ticks < 60 && finalized() < len(staged) {
		r, err := d.Tick(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if ticks == 0 {
			first = r
		}
		ticks++
	}
	if ticks != 1 {
		t.Fatalf("finalizing %d staged receipts among 50 relationships took %d ticks; want 1", len(staged), ticks)
	}
	if first.Observed != len(staged) {
		t.Errorf("observed %d, want %d", first.Observed, len(staged))
	}
	// Nothing else was read, and the 45 relationships with nothing to do were left exactly as they were.
	if len(host.reads) != len(staged) {
		t.Errorf("host reads %v, want only the %d staged anchors", host.reads, len(staged))
	}
	if n := count(t, s, "SELECT COUNT(*) FROM poll_observations"); n != len(staged) {
		t.Errorf("poll rows %d, want %d", n, len(staged))
	}
	if n := count(t, s, "SELECT COUNT(*) FROM relationships WHERE status='active' AND superseded_by IS NULL"); n != 50 {
		t.Errorf("active relationships %d, want 50 untouched", n)
	}
}

// c2: when more relationships have work than the budget allows, the longest-waiting are served first and none
// starves.
func TestWorkBeyondTheBudgetIsServedLongestWaitingFirstAndNoneStarves(t *testing.T) {
	ctx, s := throughputStore(t)
	members := []int{}
	for i := range 12 {
		seedIndexed(t, s, i)
		members = append(members, i)
	}
	host := &observationHost{status: "inProgress"} // every turn keeps running, so none ever settles
	d := New(s, host, movingClock(), nil)
	d.Policy.MaxTurnReads, d.Policy.MaxSends = 4, -1
	reads := perTick(t, ctx, d, host, 12)
	for k, tick := range reads {
		if len(tick) != 4 {
			t.Fatalf("tick %d read %v, want the budget of 4 spent on 4 relationships", k, tick)
		}
	}
	withinTicks(t, "12 relationships, budget 4", reads, members, 3)
	longestWaitingFirst(t, reads, members)
}

// Staged claims are served first, but a standing set of them cannot starve the other work: the longest-waiting
// of the rest is visited first, once, before the staged claims take the remaining budget.
func TestStagedClaimsAreServedFirstWithoutStarvingOtherWork(t *testing.T) {
	ctx, s := throughputStore(t)
	staged, plain := []int{}, []int{}
	for i := range 12 {
		seedIndexed(t, s, i)
		if i%2 == 0 {
			stageClaim(t, s, i, name("anchor", i)) // the child is still in the turn it staged the claim from
			staged = append(staged, i)
		} else {
			plain = append(plain, i)
		}
	}
	host := &observationHost{status: "inProgress"}
	d := New(s, host, movingClock(), nil)
	d.Policy.MaxTurnReads, d.Policy.MaxSends = 4, -1
	reads := perTick(t, ctx, d, host, 12)
	for k, tick := range reads {
		// The one held-back visit, then three staged relationships.
		for n, i := range tick {
			if slices.Contains(staged, i) != (n > 0) {
				t.Fatalf("tick %d read %v: want one other relationship and then three staged ones, in that order", k, tick)
			}
		}
		if len(tick) != 4 {
			t.Fatalf("tick %d read %v, want the budget of 4", k, tick)
		}
	}
	withinTicks(t, "staged claims", reads, staged, 2)
	withinTicks(t, "other work", reads, plain, 6)
	longestWaitingFirst(t, reads, staged)
	longestWaitingFirst(t, reads, plain)
}

// With nothing but staged claims, every read of the budget goes to them.
func TestStagedClaimsTakeTheWholeBudgetWhenNothingElseWaits(t *testing.T) {
	ctx, s := throughputStore(t)
	for i := range 6 {
		seedIndexed(t, s, i)
		stageClaim(t, s, i, name("anchor", i))
	}
	host := &observationHost{status: "inProgress"}
	d := New(s, host, movingClock(), nil)
	d.Policy.MaxTurnReads, d.Policy.MaxSends = 4, -1
	if reads := perTick(t, ctx, d, host, 1); len(reads[0]) != 4 {
		t.Fatalf("read %v, want the whole budget of 4", reads[0])
	}
}

// The order is derived from the store, so a restarted daemon carries on where the last one stopped.
func TestObservationOrderSurvivesARestart(t *testing.T) {
	ctx, s := throughputStore(t)
	for i := range 6 {
		seedIndexed(t, s, i)
	}
	host := &observationHost{status: "inProgress"}
	clock := movingClock()
	policy := func(d *Daemon) *Daemon { d.Policy.MaxTurnReads, d.Policy.MaxSends = 3, -1; return d }
	first := perTick(t, ctx, policy(New(s, host, clock, nil)), host, 1)[0]
	second := perTick(t, ctx, policy(New(s, host, clock, nil)), host, 1)[0] // a new Daemon on the same store
	slices.Sort(first)
	slices.Sort(second)
	if !slices.Equal(first, []int{0, 1, 2}) || !slices.Equal(second, []int{3, 4, 5}) {
		t.Fatalf("first run read %v, the restarted one %v; want the other three", first, second)
	}
}

// Reads that fail still count as attempts, so a turn the host cannot answer does not hold the front of the line.
type failingHost struct {
	*observationHost
	fail map[string]bool
}

func (h *failingHost) ReadTurn(thread, turn string) (*delivery.TurnInfo, error) {
	if h.fail[turn] {
		h.reads = append(h.reads, turn)
		return nil, errors.New("host unavailable")
	}
	return h.observationHost.ReadTurn(thread, turn)
}

func TestTurnsTheHostCannotAnswerDoNotHoldTheFrontOfTheLine(t *testing.T) {
	ctx, s := throughputStore(t)
	members := []int{}
	for i := range 6 {
		seedIndexed(t, s, i)
		members = append(members, i)
	}
	base := &observationHost{status: "inProgress"}
	host := &failingHost{observationHost: base, fail: map[string]bool{"anchor-00": true, "anchor-01": true}}
	d := New(s, host, movingClock(), nil)
	d.Policy.MaxTurnReads, d.Policy.MaxSends = 2, -1
	reads := perTick(t, ctx, d, base, 6)
	withinTicks(t, "failing reads", reads, members, 3)
}

// The budget follows the work: five relationships with something to read cost five reads, and thirty-two is
// the most any tick spends however many wait.
func TestTheBudgetFollowsTheWorkAndIsBounded(t *testing.T) {
	for _, c := range []struct{ relationships, reads int }{{5, 5}, {50, 32}} {
		t.Run(fmt.Sprint(c.relationships, " relationships"), func(t *testing.T) {
			ctx, s := throughputStore(t)
			for i := range c.relationships {
				seedIndexed(t, s, i)
			}
			host := &observationHost{status: "inProgress"}
			d := New(s, host, movingClock(), nil)
			d.Policy.MaxSends = -1
			if reads := perTick(t, ctx, d, host, 1); len(reads[0]) != c.reads {
				t.Fatalf("a tick read %d turns, want %d", len(reads[0]), c.reads)
			}
		})
	}
}

// A relationship with more pending turns than it may read in a tick reads at most its share, longest-waiting turn
// first, so every one of its turns comes round, the current anchor with the others; the budget it leaves is the
// others'. The budget is shared by the relationships that need it, so with two of them each may take half of it.
func TestARelationshipWithManyPendingTurnsReadsItsShareAndRotatesThem(t *testing.T) {
	for _, c := range []struct {
		name   string
		share  int
		within int // every one of its four turns is read in every window of this many ticks
	}{{"share of two", 2, 2}, {"share of one", 1, 4}} {
		t.Run(c.name, func(t *testing.T) {
			ctx, s := throughputStore(t)
			seedIndexed(t, s, 0)
			seedIndexed(t, s, 1)
			turns := []string{"anchor-00", "business", "continuation", "review"}
			for _, turn := range turns[1:] {
				admitTurn(t, s, 0, 1, "anchor-00", turn)
			}
			host := &observationHost{status: "inProgress"}
			d := New(s, host, movingClock(), nil)
			d.Policy.MinRelationshipShare, d.Policy.MaxTurnReads, d.Policy.MaxSends = c.share, 2*c.share, -1
			var perTickReads [][]string
			for range 12 {
				before := len(host.reads)
				if _, err := d.Tick(ctx); err != nil {
					t.Fatal(err)
				}
				perTickReads = append(perTickReads, slices.Clone(host.reads[before:]))
			}
			for _, turn := range turns {
				for start := 0; start+c.within <= len(perTickReads); start++ {
					found := false
					for _, tick := range perTickReads[start : start+c.within] {
						found = found || slices.Contains(tick, turn)
					}
					if !found {
						t.Fatalf("%s was not read in ticks %d..%d (per tick: %v)", turn, start, start+c.within-1, perTickReads)
					}
				}
			}
			for k, tick := range perTickReads {
				mine := 0
				for _, turn := range tick {
					if slices.Contains(turns, turn) {
						mine++
					}
				}
				if mine != c.share || !slices.Contains(tick, "anchor-01") {
					t.Fatalf("tick %d read %v: want %d reads for the relationship with four turns and one for the other", k, tick, c.share)
				}
			}
		})
	}
}

// A relationship with nothing to observe is not visited, writes nothing, and is visited again the moment
// the store shows work: the decision is made from store rows on every tick.
func TestAnIdleRelationshipIsLeftAloneAndPickedUpWhenWorkAppears(t *testing.T) {
	for _, c := range []struct {
		name string
		work func(t *testing.T, s *store.Store)
		read []string // turns the next tick reads; empty when the relationship stays idle
	}{
		{"a claim is staged on its settled anchor", func(t *testing.T, s *store.Store) { stageClaim(t, s, 0, "anchor-00") }, []string{"anchor-00"}},
		{"a turn is admitted and not settled", func(t *testing.T, s *store.Store) { admitTurn(t, s, 0, 1, "anchor-00", "business") }, []string{"business"}},
		{"a revision opens a generation on a new anchor", func(t *testing.T, s *store.Store) {
			exec(t, s, "INSERT INTO generations(relationship_id,execution_generation,dispatch_request_id,anchor_state,dispatch_turn_id,reason,opened_at,bound_at) VALUES('rel-00',2,'dispatch-2','bound','revision-anchor','revision','2023-11-14T22:13:20Z','2023-11-14T22:13:20Z')")
			exec(t, s, "UPDATE relationships SET execution_generation=2 WHERE relationship_id='rel-00'")
		}, []string{"revision-anchor"}},
		{"an older generation's anchor was never settled", func(t *testing.T, s *store.Store) {
			exec(t, s, "UPDATE generations SET dispatch_turn_id='old-anchor' WHERE relationship_id='rel-00'")
			exec(t, s, "INSERT INTO generations(relationship_id,execution_generation,dispatch_request_id,anchor_state,dispatch_turn_id,reason,opened_at,bound_at) VALUES('rel-00',2,'dispatch-2','bound','anchor-00','revision','2023-11-14T22:13:20Z','2023-11-14T22:13:20Z')")
			exec(t, s, "UPDATE relationships SET execution_generation=2 WHERE relationship_id='rel-00'")
		}, []string{"old-anchor"}},
		{"control: an admitted turn that is already settled", func(t *testing.T, s *store.Store) {
			admitTurn(t, s, 0, 1, "anchor-00", "business")
			settleTurn(t, s, 0, "business")
		}, nil},
		{"control: a turn whose admission names another anchor", func(t *testing.T, s *store.Store) { admitTurn(t, s, 0, 1, "some-other-anchor", "stray") }, nil},
		{"control: the relationship is paused", func(t *testing.T, s *store.Store) {
			stageClaim(t, s, 0, "anchor-00")
			exec(t, s, "UPDATE relationships SET status='paused' WHERE relationship_id='rel-00'")
		}, nil},
	} {
		t.Run(c.name, func(t *testing.T) {
			ctx, s := throughputStore(t)
			for i := range 2 {
				seedIndexed(t, s, i)
				settleTurn(t, s, i, name("anchor", i))
			}
			host := &observationHost{status: "completed"}
			d := New(s, host, &delivery.FakeClock{T: 1700000000}, nil)
			d.Policy.MaxSends = -1
			if r, err := d.Tick(ctx); err != nil || !r.Quiet() || len(host.reads) != 0 {
				t.Fatalf("idle tick: %+v reads %v err %v", r, host.reads, err)
			}
			c.work(t, s)
			if _, err := d.Tick(ctx); err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(host.reads, c.read) && !(len(c.read) == 0 && len(host.reads) == 0) {
				t.Fatalf("read %v, want %v", host.reads, c.read)
			}
		})
	}
}

// Two relationships can watch one child turn; a claim staged for one leaves the other with nothing to observe.
func TestAClaimForOneRelationshipDoesNotKeepAnotherOnTheSameTurnInTheRotation(t *testing.T) {
	ctx, s := throughputStore(t)
	seed(t, s, "r1", "parent-one", "shared-child", "anchor")
	seed(t, s, "r2", "parent-two", "shared-child", "anchor")
	exec(t, s, "INSERT INTO assignment_settlements(relationship_id,thread_id,turn_id,terminal_status,settled_at) VALUES('r2','shared-child','anchor','completed','2023-11-14T22:13:20Z')")
	exec(t, s, "INSERT INTO events(event_id,relationship_id,execution_generation,revision_hash,outcome,producer,turn_thread_id,turn_id,turn_status,receipt,stage,staged_at,first_seen_at,last_seen_at) VALUES('staged-r1','r1',1,'rev','ready_for_review','child','shared-child','anchor','inProgress','{}','staged','2023-11-14T22:13:20Z','2023-11-14T22:13:20Z','2023-11-14T22:13:20Z')")
	host := &observationHost{status: "completed"}
	d := New(s, host, &delivery.FakeClock{T: 1700000000}, nil)
	d.Policy.MaxSends = -1
	if _, err := d.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if len(host.reads) != 1 {
		t.Fatalf("reads %v, want one for r1", host.reads)
	}
	if n := count(t, s, "SELECT COUNT(*) FROM events WHERE event_id='staged-r1' AND stage='final'"); n != 1 {
		t.Fatalf("the claim was not finalized")
	}
	if n := count(t, s, "SELECT COUNT(*) FROM poll_observations WHERE relationship_id='r2'"); n != 0 {
		t.Fatalf("r2 was polled")
	}
}

// An admitted turn that ended without a receipt is found by its own relationship's query, however many
// admission rows of other relationships come before it. The old pass paged the whole table in rowid order,
// a few rows per visit.
func TestAnAdmittedTurnIsFoundHoweverManyOtherAdmissionsPrecedeIt(t *testing.T) {
	ctx, s := throughputStore(t)
	for i := range 30 {
		seedIndexed(t, s, i)
		settleTurn(t, s, i, name("anchor", i))
		for j := range 5 {
			turn := fmt.Sprintf("old-%d", j)
			admitTurn(t, s, i, 1, name("anchor", i), turn)
			settleTurn(t, s, i, turn)
		}
	}
	seedIndexed(t, s, 30)
	settleTurn(t, s, 30, name("anchor", 30))
	admitTurn(t, s, 30, 1, name("anchor", 30), "ended-without-a-receipt")
	host := &observationHost{status: "completed"}
	d := New(s, host, &delivery.FakeClock{T: 1700000000}, nil)
	d.Policy.MaxSends = -1
	if _, err := d.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(host.reads, []string{"ended-without-a-receipt"}) {
		t.Fatalf("reads %v, want the one admitted turn in the first tick", host.reads)
	}
	if n := count(t, s, "SELECT COUNT(*) FROM assignment_settlements WHERE turn_id='ended-without-a-receipt'"); n != 1 {
		t.Fatalf("the turn was not settled")
	}
}

func TestStagedClaimsOfAnInactiveRelationshipAreLeftUntouched(t *testing.T) {
	ctx, s := throughputStore(t)
	seedIndexed(t, s, 0)
	stageClaim(t, s, 0, "anchor-00")
	exec(t, s, "UPDATE relationships SET status='paused' WHERE relationship_id='rel-00'")
	host := &observationHost{status: "completed"}
	d := New(s, host, &delivery.FakeClock{T: 1700000000}, nil)
	d.Policy.MaxSends = -1
	if _, err := d.Tick(ctx); err != nil || len(host.reads) != 0 {
		t.Fatalf("reads %v err %v", host.reads, err)
	}
	if n := count(t, s, "SELECT COUNT(*) FROM events WHERE stage='staged'"); n != 1 {
		t.Fatal("the claim of a paused relationship was resolved")
	}
}

// A tick with nothing to observe is quiet in the store too: it writes no cursor, poll or journal row.
func TestATickWithNothingToObserveWritesNothing(t *testing.T) {
	ctx, s := throughputStore(t)
	for i := range 3 {
		seedIndexed(t, s, i)
		settleTurn(t, s, i, name("anchor", i))
	}
	d := New(s, &observationHost{status: "completed"}, &delivery.FakeClock{T: 1700000000}, nil)
	for range 3 {
		if _, err := d.Tick(ctx); err != nil {
			t.Fatal(err)
		}
	}
	for _, table := range []string{"poll_observations", "discovery_cursors", "journal", "observations"} {
		if n := count(t, s, "SELECT COUNT(*) FROM "+table); n != 0 {
			t.Errorf("idle ticks wrote %d rows to %s", n, table)
		}
	}
}

// With a budget of one read there is nothing to hold back for the other group, so the two are served in turn,
// longest-waiting first, and a staged claim that never ends cannot keep other work out for good.
func TestASingleReadBudgetServesBothGroupsInTurn(t *testing.T) {
	ctx, s := throughputStore(t)
	seedIndexed(t, s, 0) // plain work
	seedIndexed(t, s, 1)
	stageClaim(t, s, 1, "anchor-01") // a staged claim on a turn that keeps running
	host := &observationHost{status: "inProgress"}
	d := New(s, host, movingClock(), nil)
	d.Policy.MaxTurnReads, d.Policy.MaxSends = 1, -1
	reads := perTick(t, ctx, d, host, 8)
	withinTicks(t, "single read budget", reads, []int{0, 1}, 2)
	longestWaitingFirst(t, reads, []int{0, 1})
}

// Staged claims are read ahead of other work, apart from the one held-back visit.
func TestStagedRelationshipsAreReadBeforeOthers(t *testing.T) {
	ctx, s := throughputStore(t)
	for i := range 6 {
		seedIndexed(t, s, i)
		if i >= 4 {
			stageClaim(t, s, i, name("anchor", i))
		}
	}
	host := &observationHost{status: "inProgress"}
	d := New(s, host, movingClock(), nil)
	d.Policy.MaxSends = -1
	reads := perTick(t, ctx, d, host, 1)[0]
	// The longest-waiting of the others is visited first, once; then the staged claims, then the rest.
	if !slices.Equal(reads, []int{0, 4, 5, 1, 2, 3}) {
		t.Fatalf("read %v, want 0 (held-back visit), the staged 4 and 5, then 1, 2 and 3", reads)
	}
}

// A relationship that gets a new admitted turn before every tick does not keep another relationship waiting,
// and its own current anchor is still read among the newcomers.
func TestARelationshipThatKeepsReceivingTurnsDoesNotKeepOthersWaiting(t *testing.T) {
	ctx, s := throughputStore(t)
	seedIndexed(t, s, 0)
	seedIndexed(t, s, 1)
	clock := movingClock()
	host := &observationHost{status: "inProgress"}
	d := New(s, host, clock, nil)
	d.Policy.MaxTurnReads, d.Policy.MaxSends = 2, -1
	var reads [][]int
	anchorReads := 0
	for k := range 12 {
		admitTurnAt(t, s, 0, 1, "anchor-00", fmt.Sprintf("anchor-00-new-%d", k), clock.ISO())
		before := len(host.reads)
		reads = append(reads, perTick(t, ctx, d, host, 1)[0])
		if slices.Contains(host.reads[before:], "anchor-00") {
			anchorReads++
		}
	}
	withinTicks(t, "the other relationship", reads, []int{1}, 3)
	if anchorReads < 2 {
		t.Errorf("the current anchor was read %d times in 12 ticks; reads per tick %v", anchorReads, reads)
	}
}

// A staged claim is read for itself, not as one of its relationship's turns: three relationships that each have a
// running anchor and a staged claim on a turn that ended get all three claims finalized in the first tick of a
// budget of four, the fourth read going to the anchor held back for the other class.
func TestAStagedClaimIsReadForItselfNotAsOneOfItsRelationshipsTurns(t *testing.T) {
	ctx, s := throughputStore(t)
	statuses := map[string]string{}
	for i := range 3 {
		seedIndexed(t, s, i)
		ended := name("ended", i)
		admitTurn(t, s, i, 1, name("anchor", i), ended)
		stageClaim(t, s, i, ended)
		statuses[name("anchor", i)], statuses[ended] = "inProgress", "completed"
	}
	host := &observationHost{statuses: statuses}
	d := New(s, host, movingClock(), nil)
	d.Policy.MaxTurnReads, d.Policy.MaxSends = 4, -1
	if _, err := d.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if n := count(t, s, "SELECT COUNT(*) FROM events WHERE stage='final'"); n != 3 {
		t.Fatalf("%d claims finalized in the first tick, want 3; reads %v", n, host.reads)
	}
}

// A relationship can have a turn of each class. The classes rank its relationships separately, so reading its
// staged turn does not stand in for reading its other one, and with a share of one the read held back for the
// other class does not cost it its staged read.
func TestARelationshipWithATurnOfEachClassHasBothReadInTurn(t *testing.T) {
	for _, share := range []int{1, 2} {
		t.Run(fmt.Sprint("share of ", share), func(t *testing.T) {
			ctx, s := throughputStore(t)
			seedIndexed(t, s, 0)
			seedIndexed(t, s, 1)
			admitTurn(t, s, 0, 1, "anchor-00", "claimed")
			stageClaim(t, s, 0, "claimed") // a staged claim waits on the admitted turn, not on the anchor
			host := &observationHost{status: "inProgress"}
			d := New(s, host, movingClock(), nil)
			d.Policy.MinRelationshipShare, d.Policy.MaxTurnReads, d.Policy.MaxSends = share, 2, -1
			var reads [][]string
			for range 8 {
				before := len(host.reads)
				if _, err := d.Tick(ctx); err != nil {
					t.Fatal(err)
				}
				reads = append(reads, slices.Clone(host.reads[before:]))
			}
			within := func(turn string, n int) {
				for start := 0; start+n <= len(reads); start++ {
					found := false
					for _, tick := range reads[start : start+n] {
						found = found || slices.Contains(tick, turn)
					}
					if !found {
						t.Fatalf("%s was not read in ticks %d..%d (per tick: %v)", turn, start, start+n-1, reads)
					}
				}
			}
			within("claimed", 1)
			within("anchor-00", 2)
			within("anchor-01", 2)
		})
	}
}

// The share of a relationship comes from the cap, not from what is pending, so budget one relationship leaves goes to
// another that needs it: with the default policy a relationship with four pending turns beside one with a single
// turn has all five read in the first tick.
func TestSpareBudgetGoesToTheRelationshipThatNeedsIt(t *testing.T) {
	ctx, s := throughputStore(t)
	seedIndexed(t, s, 0)
	seedIndexed(t, s, 1)
	for _, turn := range []string{"business", "continuation", "review"} {
		admitTurn(t, s, 0, 1, "anchor-00", turn)
	}
	host := &observationHost{status: "inProgress"}
	d := New(s, host, movingClock(), nil)
	d.Policy.MaxSends = -1
	if _, err := d.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if len(host.reads) != 5 {
		t.Fatalf("reads %v, want all five pending turns in one tick", host.reads)
	}
}

// A stamp later than the clock now was written before the clock was set back, so it is older than anything the clock
// stamps now: it must not hold the same turns at the front of the line. Without that, turns stamped in the future keep
// ranking behind the ones just read for as long as the clock stays behind them.
func TestAClockSetBackDoesNotPinTheSameTurnsAtTheFront(t *testing.T) {
	const future = "2100-01-01T00:00:00.000000+00:00"
	for _, c := range []struct {
		name          string
		relationships int
		budget        int
		setBack       func(t *testing.T, s *store.Store)
	}{
		{"turns that became pending after the clock now", 50, 32, func(t *testing.T, s *store.Store) {
			exec(t, s, "UPDATE generations SET opened_at=?, bound_at=?", future, future)
		}},
		{"turns last read after the clock now", 50, 32, func(t *testing.T, s *store.Store) {
			exec(t, s, "INSERT INTO poll_observations(relationship_id,execution_generation,turn_id,last_status,last_polled_at,last_attempt_at) SELECT relationship_id,execution_generation,dispatch_turn_id,'inProgress',?,? FROM generations", future, future)
		}},
		{"three relationships, a budget of two", 3, 2, func(t *testing.T, s *store.Store) {
			exec(t, s, "INSERT INTO poll_observations(relationship_id,execution_generation,turn_id,last_status,last_polled_at,last_attempt_at) SELECT relationship_id,execution_generation,dispatch_turn_id,'inProgress',?,? FROM generations", future, future)
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			ctx, s := throughputStore(t)
			members := []int{}
			for i := range c.relationships {
				seedIndexed(t, s, i)
				members = append(members, i)
			}
			c.setBack(t, s)
			host := &observationHost{status: "inProgress"}
			d := New(s, host, movingClock(), nil)
			d.Policy.MaxTurnReads, d.Policy.MaxSends = c.budget, -1
			rounds := (c.relationships + c.budget - 1) / c.budget
			reads := perTick(t, ctx, d, host, 2*rounds)
			withinTicks(t, "clock set back", reads, members, rounds+1)
		})
	}
}
