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

// Every writer of a terminal row of an agent call decides the directory of the call's own by one rule (ownsDirectoryAt): the
// superseded rows a resume writes for an attempt an earlier process left (supersedeSuffix, endOpenCalls) read it from the attempt's
// prepared row, name it on the row and remove it after the row; an attempt that is no pinned architect continuation names none.
func TestEveryTerminalRowWriterDecidesTheOwnDirectory(t *testing.T) {
	prepared := func(data map[string]any) []Event {
		return []Event{{Kind: KindCall, Op: "agent", Event: EvPrepared, Data: data}}
	}
	pinned := map[string]any{"role": "architect", "resume_of": "c_arch", "head": "abc", "allocation": map[string]any{"kind": PlaceCall, "path": "/s/calls/c1/wt"}}
	with := func(key string, value any) map[string]any {
		out := map[string]any{}
		for k, v := range pinned {
			out[k] = v
		}
		out[key] = value
		return out
	}
	for _, testCase := range []struct {
		name string
		data map[string]any
		want string
	}{
		{"pinned architect continuation", pinned, "/s/calls/c1/wt"},
		{"no head", with("head", ""), ""},
		{"not a continuation", with("resume_of", ""), ""},
		{"another role", with("role", "reviewer"), ""},
		{"a QA turn", with("qa", true), ""},
		{"a continued directory", with("allocation", map[string]any{"kind": PlaceContinued, "path": "/s/wt/A"}), ""},
	} {
		if got := ownDirectoryOfRows(prepared(testCase.data)); got != testCase.want {
			t.Errorf("%s: %q, want %q", testCase.name, got, testCase.want)
		}
	}

	j := NewMemoryJournal()
	backend := &removerBackend{Fake: NewFake()}
	e := &Engine{cfg: Config{Backend: backend}, j: j, runID: "run-1", ctx: context.Background()}
	first := Event{Path: "root", Position: 3, Label: "reflection:M1"}
	e.supersededRow("c1", prepared(pinned), first, map[string]any{"reason": ReasonSupersededByResume})
	e.supersededRow("c2", prepared(with("role", "reviewer")), first, map[string]any{"reason": ReasonSupersededByResume})
	rows := j.Events()
	if len(rows) != 2 || rows[0].Event != EvSuperseded || rowStr(rows[0].Data, ownDirectoryKey) != "/s/calls/c1/wt" || rowStr(rows[1].Data, ownDirectoryKey) != "" {
		t.Fatalf("superseded rows: %+v", rows)
	}
	if !slices.Equal(backend.removed, []string{"/s/calls/c1/wt"}) {
		t.Errorf("removed %v after the superseded rows", backend.removed)
	}
}
