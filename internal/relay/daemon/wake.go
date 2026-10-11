package daemon

import (
	"context"
	"slices"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/delivery"
)

// MinWakeGap is the shortest time between the starts of two ticks that wakes ended: the debounce floor
// that keeps the per-tick budgets true however many Stops arrive (CRW-1210, R1).
const MinWakeGap = time.Second

// Waker is the daemon loop's wake channel. The Stop hook's control handler lives in this process
// (control and daemon are one, internal/relay/cli/daemon.go), and posts here the thread of a managed child
// whose turn ended; RunWoken ends its wait for it. It is an in-process channel, never a socket method
// (decision 25), and posting never blocks: a full channel drops the wake, which the poll interval's tick
// (the backstop) makes up.
type Waker struct {
	// Gap is the debounce between ticks that wakes ended; never below MinWakeGap.
	Gap time.Duration
	ch  chan string
}

// wakeBuffer is how many posted wakes wait for the loop: the tens of managed children that end turns in a
// second, with room for a burst.
const wakeBuffer = 256

func NewWaker() *Waker { return &Waker{Gap: MinWakeGap, ch: make(chan string, wakeBuffer)} }

// Wake posts thread. It never blocks, and a nil Waker or an empty thread is ignored.
func (w *Waker) Wake(thread string) {
	if w == nil || thread == "" {
		return
	}
	select {
	case w.ch <- thread:
	default:
	}
}

func (w *Waker) gap() time.Duration { return max(w.Gap, MinWakeGap) }

// drain takes every wake already posted, each thread once, in the order first posted.
func (w *Waker) drain(woken []string) []string {
	for {
		select {
		case thread := <-w.ch:
			if !slices.Contains(woken, thread) {
				woken = append(woken, thread)
			}
		default:
			return woken
		}
	}
}

type wokenKey struct{}

// WithWoken is ctx carrying the threads that woke the tick it is passed to: observe reads their turns first.
func WithWoken(ctx context.Context, threads []string) context.Context {
	if len(threads) == 0 {
		return ctx
	}
	return context.WithValue(ctx, wokenKey{}, slices.Clone(threads))
}

// Woken are the threads that woke the tick ctx belongs to, or none for a tick the backstop ran.
func Woken(ctx context.Context) []string {
	threads, _ := ctx.Value(wokenKey{}).([]string)
	return threads
}

// RunWoken is Run for a loop that wakes: after a tick it waits for the poll interval (the backstop) or for a
// wake, whichever comes first, and a wake runs the next tick at once unless the last one began less than the
// debounce gap ago, in which case the tick waits out the gap and takes every wake that arrived meanwhile
// together. A woken tick is handed the threads that woke it (WithWoken). The bounds are Run's: maxTicks or
// deadline, with stop in addition. A nil wake is Run with the interval as its wait.
func RunWoken(ctx context.Context, tick func(context.Context) (Report, error), clock delivery.Clock, interval float64, maxTicks *int, deadline *float64, stop func() bool, wake *Waker) ([]Report, error) {
	return loop(ctx, tick, clock, maxTicks, deadline, stop, func(began time.Time) ([]string, error) {
		if wake == nil {
			return nil, SchedulerWait(ctx, clock, deadline, interval)
		}
		return wake.wait(ctx, clock, deadline, interval, began)
	})
}

// wait is the pause after a tick that began at began: the poll interval, ended early by a wake, and in
// any case held until the debounce gap since began has passed when a wake ended it.
func (w *Waker) wait(ctx context.Context, clock delivery.Clock, deadline *float64, interval float64, began time.Time) ([]string, error) {
	seconds := interval
	if deadline != nil {
		seconds = min(seconds, *deadline-clock.Now())
	}
	if seconds <= 0 {
		return w.drain(nil), nil
	}
	timer := time.NewTimer(time.Duration(seconds * float64(time.Second)))
	defer timer.Stop()
	var woken []string
	select {
	case <-timer.C:
		return w.drain(nil), nil
	case <-ctx.Done():
		return nil, ctx.Err()
	case thread := <-w.ch:
		woken = []string{thread}
	}
	if remaining := w.gap() - time.Since(began); remaining > 0 {
		hold := time.NewTimer(remaining)
		defer hold.Stop()
		select {
		case <-hold.C:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return w.drain(woken), nil
}
