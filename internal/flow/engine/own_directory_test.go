package engine

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"
)

// removerBackend records the directories the engine asks it to remove; a path in fail is not removed.
type removerBackend struct {
	*Fake
	mu      sync.Mutex
	removed []string
	fail    map[string]bool
	ctxErr  []error
}

func (b *removerBackend) RemoveDir(ctx context.Context, path string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.ctxErr = append(b.ctxErr, ctx.Err())
	if b.fail[path] {
		return errors.New("locked")
	}
	b.removed = append(b.removed, path)
	return nil
}

// The directory of a pinned architect turn goes with the terminal row that ends the call and names it: a resume removes every directory
// a terminal row names - whatever the call comes to in the resume - with a context the run's cancellation does not end, and journals a
// removal that fails (own_directory_kept) for the next resume. The directory a call is still at work in is kept: a call whose newest
// state is no terminal row, or an unknown outcome, and a held call that went on in place in the directory its held row named (CRW-1202).
func TestPinnedWorktreeSweepKeepsWhatACallIsAtWorkIn(t *testing.T) {
	j := NewMemoryJournal()
	row := func(call, event string, data map[string]any) {
		j.Append(Event{RunID: "run-1", Kind: KindCall, CallID: call, Op: "agent", Event: event, Data: data})
	}
	alloc := func(path string) map[string]any {
		return map[string]any{"role": "architect", "allocation": map[string]any{"kind": PlaceCall, "path": path}}
	}
	// completed: removed
	row("c_done", EvPrepared, alloc("/s/calls/c_done/wt"))
	row("c_done", EvCompleted, map[string]any{"own_directory": "/s/calls/c_done/wt"})
	// held, then gone on in place in the same directory and still open: kept
	row("c_held", EvPrepared, alloc("/s/calls/c_held/wt"))
	row("c_held", EvHeld, map[string]any{"own_directory": "/s/calls/c_held/wt"})
	row("c_held", EvTurnRequested, nil)
	// an earlier allocation completed, the call ran again in a directory of its own and its outcome is unknown: the earlier one goes,
	// the one at work stays
	row("c_again", EvPrepared, alloc("/s/calls/c_again/wt"))
	row("c_again", EvCompleted, map[string]any{"own_directory": "/s/calls/c_again/wt"})
	row("c_again", EvPrepared, alloc("/s/calls/c_again/wt-1"))
	row("c_again", EvOutcomeUnknown, map[string]any{})
	// a removal that fails
	row("c_locked", EvPrepared, alloc("/s/calls/c_locked/wt"))
	row("c_locked", EvRefused, map[string]any{"own_directory": "/s/calls/c_locked/wt"})

	backend := &removerBackend{Fake: NewFake(), fail: map[string]bool{"/s/calls/c_locked/wt": true}}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	e := &Engine{cfg: Config{Backend: backend}, j: j, runID: "run-1", ctx: ctx}
	e.prior = newPriorIndex(j, 1<<30)
	e.sweepOwnDirectories()

	if want := []string{"/s/calls/c_done/wt", "/s/calls/c_again/wt"}; !slices.Equal(backend.removed, want) {
		t.Errorf("removed %v, want %v", backend.removed, want)
	}
	for _, err := range backend.ctxErr {
		if err != nil {
			t.Errorf("a removal got a context that the run's cancellation ended: %v", err)
		}
	}
	kept := 0
	for _, ev := range j.Events() {
		if ev.Kind == KindFlow && ev.Event == EvOwnDirectoryKept && rowStr(ev.Data, "path") == "/s/calls/c_locked/wt" && rowStr(ev.Data, "error") != "" {
			kept++
		}
	}
	if kept != 1 {
		t.Errorf("the failed removal is journaled %d times, want once", kept)
	}
}
