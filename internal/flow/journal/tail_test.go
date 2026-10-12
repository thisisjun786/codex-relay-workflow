package journal

import (
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/flow/engine"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// TestLoadAfterPagesTheRunInOrder: reading a run page by page with the last sequence number as the cursor gives every row Load
// gives, once and in order, the page says when a row beyond it exists, and a run the store does not hold is hasRun false.
func TestLoadAfterPagesTheRunInOrder(t *testing.T) {
	st, _ := newStore(t)
	j := newJournal(t, st)
	run(t, j, engine.NewFake(), nil, `phase('one')
const a = await agent.call('build', { agentType: 'executor', node: 'n1', label: 'a' })
phase('two')
const b = await agent.call('check', { agentType: 'reviewer', node: 'n1', label: 'b' })
return [a.status, b.status]`)
	if err := j.Close(); err != nil {
		t.Fatal(err)
	}
	all, hasRun, err := Load(ctx, st, runID)
	if err != nil || !hasRun || len(all) < 8 {
		t.Fatalf("Load: %d rows, %v, %v", len(all), hasRun, err)
	}
	var got []engine.Event
	cursor := 0
	for pages := 0; ; pages++ {
		page, more, hasRun, err := LoadAfter(ctx, st, runID, cursor, 3)
		if err != nil || !hasRun {
			t.Fatalf("LoadAfter(%d): %v %v", cursor, hasRun, err)
		}
		if len(page) > 3 || (more && len(page) != 3) {
			t.Fatalf("page of %d rows, more %v", len(page), more)
		}
		got = append(got, page...)
		if len(page) > 0 {
			cursor = page[len(page)-1].Seq
		}
		if !more {
			break
		}
		if pages > len(all) {
			t.Fatal("the pages do not end")
		}
	}
	if len(got) != len(all) {
		t.Fatalf("pages hold %d rows, the run %d", len(got), len(all))
	}
	for i := range all {
		if got[i].Seq != all[i].Seq || got[i].Event != all[i].Event || got[i].CallID != all[i].CallID {
			t.Fatalf("row %d: %+v, want %+v", i, got[i], all[i])
		}
	}
	lean, _, err := LoadLean(ctx, st, runID)
	if err != nil || len(lean) != len(all) {
		t.Fatalf("LoadLean: %d rows, %v", len(lean), err)
	}
	for _, e := range lean {
		if _, has := e.Data["result"]; has {
			t.Fatalf("a lean row carries a result: %+v", e)
		}
	}
	withResult := 0
	for _, e := range all {
		if _, has := e.Data["result"]; has {
			withResult++
		}
	}
	if withResult == 0 {
		t.Fatal("the full load carries no result either, so the lean check proves nothing")
	}
	if page, more, _, err := LoadAfter(ctx, st, runID, cursor, 3); err != nil || len(page) != 0 || more {
		t.Errorf("after the last row: %d rows, more %v, %v", len(page), more, err)
	}
	if _, _, hasRun, err := LoadAfter(ctx, st, "run-none", 0, 3); err != nil || hasRun {
		t.Errorf("an unknown run: %v %v", hasRun, err)
	}
	if _, _, _, err := LoadAfter(ctx, st, runID, 0, 0); err == nil {
		t.Error("an empty page was accepted")
	}
}

// seedRun writes the rows of a run that has begun: its started row (seq 1) and one prepared row of a call (seq 2).
func seedRun(t *testing.T, st *store.Store) *Journal {
	t.Helper()
	j, err := Open(ctx, st, runID, identity, Options{Immediate: true})
	if err != nil {
		t.Fatal(err)
	}
	j.Append(engine.Event{RunID: runID, Kind: engine.KindRun, Event: "started", Data: map[string]any{"workflow": "w", "script_digest": "d", "args_digest": "a"}})
	j.Append(engine.Event{RunID: runID, Kind: engine.KindCall, CallID: "c1", Path: "0", Op: "agent", Event: engine.EvPrepared})
	if err := j.Err(); err != nil {
		t.Fatal(err)
	}
	return j
}

func readOnly(t *testing.T, path string) *store.Store {
	t.Helper()
	ro, err := store.OpenReadOnlyStore(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ro.Close() })
	return ro
}

// TestLoadAfterPageIsOneSnapshot: the page of the rows after a cursor does not skip a committed call row. The call rows and the flow
// rows are two statements; a writer that commits a call row (seq 3) and a flow row (seq 4) between them must not make the page
// hold seq 4 alone, because its cursor would then pass seq 3 for good (CRW-1205 d3).
func TestLoadAfterPageIsOneSnapshot(t *testing.T) {
	st, path := newStore(t)
	j := seedRun(t, st)
	ro := readOnly(t, path)
	fired := false
	testAfterCallsRead = func() {
		if fired {
			return
		}
		fired = true
		j.Append(engine.Event{RunID: runID, Kind: engine.KindCall, CallID: "c1", Path: "0", Op: "agent", Event: engine.EvCompleted})
		j.Append(engine.Event{RunID: runID, Kind: engine.KindRun, Event: "finished", Data: map[string]any{"status": "completed"}})
	}
	t.Cleanup(func() { testAfterCallsRead = nil })
	var seen []int
	cursor := 2
	for i := 0; i < 4; i++ {
		page, _, _, err := LoadAfter(ctx, ro, runID, cursor, 10)
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range page {
			seen = append(seen, e.Seq)
			cursor = e.Seq
		}
	}
	if !fired {
		t.Fatal("the writer never ran between the two statements")
	}
	if len(seen) != 2 || seen[0] != 3 || seen[1] != 4 {
		t.Fatalf("a follower of the cursor saw seq %v, want [3 4]", seen)
	}
}

// TestReadPageStateIsOfThePageMoment: the rows a state is summed from are those of the page's own snapshot. A run that finishes
// after an empty page was read is not reported over with that page, because a follower that stops on "over" would stop at the old
// cursor and never see the finished row (CRW-1205 d3).
func TestReadPageStateIsOfThePageMoment(t *testing.T) {
	st, path := newStore(t)
	j := seedRun(t, st)
	ro := readOnly(t, path)
	testAfterPageRead = func() {
		j.Append(engine.Event{RunID: runID, Kind: engine.KindRun, Event: "finished", Data: map[string]any{"status": "completed"}})
	}
	t.Cleanup(func() { testAfterPageRead = nil })
	p, err := ReadPage(ctx, ro, runID, 2, 10, true)
	if err != nil || !p.HasRun {
		t.Fatalf("ReadPage: %v %v", p.HasRun, err)
	}
	if len(p.Events) != 0 {
		t.Fatalf("the page holds %d rows after the last row", len(p.Events))
	}
	if len(p.Lean) != 2 {
		t.Fatalf("the rows of the state hold %d rows, the page's snapshot holds 2 (the finished row came after it)", len(p.Lean))
	}
	testAfterPageRead = nil
	next, err := ReadPage(ctx, ro, runID, 2, 10, true)
	if err != nil || len(next.Events) != 1 || next.Events[0].Seq != 3 || len(next.Lean) != 3 {
		t.Fatalf("the next page: %+v %v", next, err)
	}
}
