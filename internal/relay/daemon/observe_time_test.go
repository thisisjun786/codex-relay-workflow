package daemon

import (
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/delivery"
)

// stopwatch is the monotonic clock the observation time bound runs on, moved by hand.
type stopwatch struct{ seconds float64 }

func (w *stopwatch) now() time.Time {
	return time.Unix(1700000000, 0).Add(time.Duration(w.seconds * float64(time.Second)))
}

// timedHost is a host whose reads take time, and may fail, as a sick one does: a read can wait out the transport's
// timeout before it errors. Each read moves the stopwatch by its cost and the wall clock by wallStep.
type timedHost struct {
	*observationHost
	watch    *stopwatch
	wall     *delivery.FakeClock
	wallStep float64
	cost     func(turn string) float64
	fail     func(turn string) bool
}

func (h *timedHost) ReadTurn(thread, turn string) (*delivery.TurnInfo, error) {
	h.watch.seconds += h.cost(turn)
	h.wall.T += h.wallStep
	if h.fail != nil && h.fail(turn) {
		h.reads = append(h.reads, turn)
		return nil, errors.New("deadline exceeded")
	}
	return h.observationHost.ReadTurn(thread, turn)
}

// slowObservation seeds one relationship per entry of staged (true: a staged claim waits on its running anchor)
// and returns a function that runs one tick and says which relationships it read, how long it took on the
// stopwatch and what it noted.
func slowObservation(t *testing.T, staged []bool, cost func(string) float64, fail func(string) bool) (*Daemon, *timedHost, func() ([]int, float64, []string)) {
	t.Helper()
	ctx, s := throughputStore(t)
	for i, claim := range staged {
		seedIndexed(t, s, i)
		if claim {
			stageClaim(t, s, i, name("anchor", i))
		}
	}
	wall, watch := movingClock(), &stopwatch{}
	base := &observationHost{status: "inProgress"}
	host := &timedHost{observationHost: base, watch: watch, wall: wall, cost: cost, fail: fail}
	d := New(s, host, wall, nil)
	d.Policy.MaxSends = -1
	d.mono = watch.now
	return d, host, func() ([]int, float64, []string) {
		before, started := len(base.reads), watch.seconds
		report, err := d.Tick(ctx)
		if err != nil {
			t.Fatal(err)
		}
		var served []int
		for _, turn := range base.reads[before:] {
			served = append(served, indexOf(turn))
		}
		return served, watch.seconds - started, report.Notes
	}
}

func noneStaged(n int) []bool { return make([]bool, n) }

// A host that does not answer costs a tick at most the time bound and the read in flight, not the whole budget of
// reads: each read can wait out the transport timeout, and the sweep and the deliveries wait behind them.
func TestASlowHostCostsATickAtMostTheTimeBoundPlusOneRead(t *testing.T) {
	_, _, tick := slowObservation(t, noneStaged(10), func(string) float64 { return 4 }, func(string) bool { return true })
	first, elapsed, notes := tick()
	second, _, _ := tick()
	if !slices.Equal(first, []int{0, 1, 2}) || !slices.Equal(second, []int{3, 4, 5}) {
		t.Fatalf("a tick read %v and the next %v; want three slow reads each, on different relationships", first, second)
	}
	if elapsed > 10+4 {
		t.Errorf("the tick took %v s, want at most the 10 s bound and the read in flight", elapsed)
	}
	stopped := 0
	for _, note := range notes {
		if strings.HasPrefix(note, "observation stopped after") {
			stopped++
		}
	}
	if stopped != 1 {
		t.Errorf("notes %q: want one saying why the pass stopped", notes)
	}
}

// The bound is on elapsed time, measured on a monotonic clock: a wall clock that steps back inside every read
// neither hides the time spent nor lengthens the pass.
func TestTheTimeBoundIgnoresAWallClockThatStepsBack(t *testing.T) {
	_, host, tick := slowObservation(t, noneStaged(10), func(string) float64 { return 4 }, func(string) bool { return true })
	host.wallStep = -120
	if served, elapsed, _ := tick(); len(served) != 3 || elapsed > 14 {
		t.Fatalf("read %v in %v s, want three reads", served, elapsed)
	}
}

// Slow reads the host does answer, and slow and fast reads in turn, stay inside the bound too: it is time, not a
// count of failures, that is spent.
func TestSlowReadsInTurnWithFastOnesStayInsideTheTimeBound(t *testing.T) {
	cost := func(turn string) float64 {
		if indexOf(turn)%2 == 0 {
			return 4
		}
		return 0
	}
	_, _, tick := slowObservation(t, noneStaged(20), cost, func(turn string) bool { return indexOf(turn)%2 == 0 })
	served, elapsed, _ := tick()
	// Reads cost 4, 0, 4, 0, 4 s: the pass stops before the sixth, at 12 s.
	if elapsed > 10+4 || len(served) != 5 {
		t.Fatalf("read %v in %v s, want five reads, 12 s, inside the 10 s bound and one read", served, elapsed)
	}
}

func TestZeroMaxObserveSecondsSpendsTheWholeBudgetOnASlowHost(t *testing.T) {
	d, _, tick := slowObservation(t, noneStaged(10), func(string) float64 { return 4 }, func(string) bool { return true })
	d.Policy.MaxObserveSeconds = 0
	if served, _, _ := tick(); len(served) != 10 {
		t.Fatalf("read %v, want all ten", served)
	}
}

// A slow class cannot keep the other out of the tick: the first read of each class is made whatever the clock says,
// and the classes rotate their relationships, so every one of them comes round.
func TestASlowClassDoesNotKeepTheOtherOutOfTheTick(t *testing.T) {
	staged := []bool{false, false, false, true, true, true}
	for _, c := range []struct {
		name string
		slow func(i int) bool // reads of these relationships take longer than the whole bound
	}{
		{"slow staged relationships", func(i int) bool { return i >= 3 }},
		{"slow other relationships", func(i int) bool { return i < 3 }},
	} {
		t.Run(c.name, func(t *testing.T) {
			cost := func(turn string) float64 {
				if c.slow(indexOf(turn)) {
					return 11
				}
				return 0
			}
			_, _, tick := slowObservation(t, staged, cost, nil)
			var reads [][]int
			for range 6 {
				served, _, _ := tick()
				reads = append(reads, served)
			}
			withinTicks(t, "other work", reads, []int{0, 1, 2}, 3)
			withinTicks(t, "staged claims", reads, []int{3, 4, 5}, 3)
		})
	}
}

// With both classes slow the pass still makes its two first reads, one for each class, and nothing more: the bound
// is the limit plus the read in flight, or those two reads if they alone take longer.
func TestBothClassesSlowStillGetTheirFirstReadAndNothingMore(t *testing.T) {
	staged := []bool{false, false, false, true, true, true}
	_, _, tick := slowObservation(t, staged, func(string) float64 { return 11 }, nil)
	var reads [][]int
	for range 6 {
		served, elapsed, _ := tick()
		if len(served) != 2 || elapsed != 22 {
			t.Fatalf("read %v in %v s, want the first read of each class and 22 s", served, elapsed)
		}
		reads = append(reads, served)
	}
	withinTicks(t, "other work", reads, []int{0, 1, 2}, 3)
	withinTicks(t, "staged claims", reads, []int{3, 4, 5}, 3)
}
