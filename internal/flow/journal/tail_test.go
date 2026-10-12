package journal

import (
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/flow/engine"
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
