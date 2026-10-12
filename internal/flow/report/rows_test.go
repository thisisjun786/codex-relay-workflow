package report

import (
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/flow/engine"
)

// A row a command writes into a run that is alive takes its number from the foreign range, so the run's own journal goes on writing
// without meeting it, and a reader finds both in the order they were written.
func TestForeignRowsNeverCollideWithTheRunsJournal(t *testing.T) {
	st := newStore(t)
	j := seedRun(t, st, "run-1", "plan-1", "orch")
	j.Append(engine.Event{RunID: "run-1", Kind: engine.KindFlow, Event: "log", Data: map[string]any{"text": "one"}})
	if err := j.Flush(); err != nil {
		t.Fatal(err)
	}
	app := ForeignAppender{Store: st, Run: "run-1", Now: fixedNow}
	s1, err := app.Append(ctx, NewRow{Kind: KindReport, Payload: map[string]any{"report_id": "rep_1"}})
	if err != nil || s1 != ForeignSeqBase {
		t.Fatalf("first foreign row: seq %d err %v", s1, err)
	}
	// the run goes on after the command wrote
	j.Append(engine.Event{RunID: "run-1", Kind: engine.KindFlow, Event: "log", Data: map[string]any{"text": "two"}})
	if err := j.Flush(); err != nil || j.Err() != nil {
		t.Fatalf("the journal met the foreign row: %v %v", err, j.Err())
	}
	s2, err := app.Append(ctx, NewRow{Kind: KindDelivery, Payload: map[string]any{"report_id": "rep_1", "result": "queued"}})
	if err != nil || s2 != ForeignSeqBase+1 {
		t.Fatalf("second foreign row: seq %d err %v", s2, err)
	}
	rows, err := ReadRows(ctx, st.DB, "run-1", KindReport, KindDelivery)
	if err != nil || len(rows) != 2 || rows[0].Kind != KindReport || rows[1].Str("result") != "queued" {
		t.Fatalf("rows %+v err %v", rows, err)
	}
	// a resumed journal reads the foreign rows as flow rows and carries on after them
	j2 := seedRunOpen(t, st, "run-1")
	var kinds []string
	for _, e := range j2.Events() {
		if e.Kind == engine.KindFlow {
			kinds = append(kinds, e.Event)
		}
	}
	if len(kinds) != 4 {
		t.Fatalf("the resumed journal holds flow rows %v", kinds)
	}
}

func TestForeignRowOnceWritesAKeyOnce(t *testing.T) {
	st := newStore(t)
	seedRun(t, st, "run-1", "plan-1", "orch")
	app := ForeignAppender{Store: st, Run: "run-1", Now: fixedNow}
	row := NewRow{Kind: KindSummaryNotice, Payload: map[string]any{"summary_id": "sum-1"}}
	w1, _, err := app.AppendOnce(ctx, row, "summary_id")
	if err != nil || !w1 {
		t.Fatalf("first: %v %v", w1, err)
	}
	w2, _, err := app.AppendOnce(ctx, row, "summary_id")
	if err != nil || w2 {
		t.Fatalf("second wrote again: %v %v", w2, err)
	}
	if _, _, err := app.AppendOnce(ctx, NewRow{Kind: KindSummaryNotice, Payload: map[string]any{"summary_id": "sum-2"}}, "summary_id"); err != nil {
		t.Fatal(err)
	}
	rows, _ := ReadRows(ctx, st.DB, "", KindSummaryNotice)
	if len(rows) != 2 {
		t.Fatalf("rows %+v", rows)
	}
}

func TestForeignRowNeedsARunAndNoReservedMember(t *testing.T) {
	st := newStore(t)
	app := ForeignAppender{Store: st, Run: "run-none", Now: fixedNow}
	if _, err := app.Append(ctx, NewRow{Kind: KindReport, Payload: map[string]any{"a": 1}}); err == nil {
		t.Fatal("a row was written into a run the store does not hold")
	}
	seedRun(t, st, "run-1", "plan-1", "orch")
	app.Run = "run-1"
	if _, err := app.Append(ctx, NewRow{Kind: KindReport, Payload: map[string]any{"_kind": "x"}}); err == nil {
		t.Fatal("a reserved member was accepted")
	}
}
