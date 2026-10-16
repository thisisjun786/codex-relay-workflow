package engine

import (
	"context"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/flow/script"
)

// lingerOp is an op that tells the test it started, then keeps running for `linger` after its context is done (a verify tree
// that holds its pipes past the cancel), and ends with the context's error.
type lingerOp struct {
	started chan<- string
	linger  time.Duration
}

func (o lingerOp) Run(ctx context.Context, req OpRequest) (OpResult, error) {
	o.started <- req.CallID
	<-ctx.Done()
	time.Sleep(o.linger)
	return OpResult{}, ctx.Err()
}
func (lingerOp) Proven(context.Context, []Event, Event) (bool, string, error) { return true, "", nil }

// quickOp completes at once.
type quickOp struct{}

func (quickOp) Run(context.Context, OpRequest) (OpResult, error) { return OpResult{}, nil }
func (quickOp) Proven(context.Context, []Event, Event) (bool, string, error) {
	return true, "", nil
}

func loadCancelScript(t *testing.T, body string) *script.Program {
	t.Helper()
	prog, err := script.Load(script.Source{Name: "t.js", Text: []byte("export const meta = { name: 't' }\n" + body)})
	if err != nil {
		t.Fatal(err)
	}
	return prog
}

func waitingRows(events []Event) []Event {
	var out []Event
	for _, ev := range events {
		if ev.Kind == KindFlow && ev.Event == EvCancelWaiting {
			out = append(out, ev)
		}
	}
	return out
}

// TestCancelWritesTheOpsInFlight (CRW-1573, D3.3): two ops are in flight when the run is cancelled and both outlive the cancel
// by 2 s. The run ends cancelled, and its rows hold one flow cancel_waiting row naming both ops (call id, op, node: the args'
// node member, or the first argument when it is a string), sorted by call id, written before either op's refused row and
// before the run's finished row.
func TestCancelWritesTheOpsInFlight(t *testing.T) {
	prog := loadCancelScript(t, "await parallel([async () => crw.verify({ node: 'n1' }), async () => crw.integrate('n2', 'head')])\nreturn 1\n")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	started := make(chan string, 2)
	ops := map[string]Op{"verify": lingerOp{started, 2 * time.Second}, "integrate": lingerOp{started, 2 * time.Second}}
	go func() {
		defer cancel()
		for i := 0; i < 2; i++ {
			select {
			case <-started:
			case <-time.After(30 * time.Second):
				return
			}
		}
	}()
	j := &flushJournal{MemoryJournal: NewMemoryJournal()}
	res, err := Run(ctx, prog, Config{RunID: "run-cw", Backend: NewFake(), Policy: FakePolicy(), Journal: j, Ops: ops})
	if err != nil {
		t.Fatal(err)
	}
	if res.ErrorKind != ErrKindCancelled {
		t.Fatalf("the run did not end cancelled: %+v", res)
	}
	events := j.Events()
	rows := waitingRows(events)
	if len(rows) != 1 {
		t.Fatalf("%d cancel_waiting rows, want 1: %+v", len(rows), events)
	}
	w := rows[0]
	refused := map[string]Event{}
	ids := map[string]string{}
	finished := -1
	for _, ev := range events {
		if ev.Kind == KindCall && ev.Event == EvRefused {
			refused[ev.Op] = ev
			ids[ev.Op] = ev.CallID
		}
		if ev.Kind == KindRun && ev.Event == "finished" {
			finished = ev.Seq
		}
	}
	if len(refused) != 2 {
		t.Fatalf("refused rows %+v, want one per op", refused)
	}
	for op, r := range refused {
		if r.Seq < w.Seq {
			t.Errorf("the refused row of %s (seq %d) precedes the cancel_waiting row (seq %d)", op, r.Seq, w.Seq)
		}
	}
	first := finished
	for _, r := range refused {
		if r.Seq < first {
			first = r.Seq
		}
	}
	if !j.flushedBetween(w.Seq, first) {
		t.Errorf("the cancel_waiting row (seq %d) was not flushed before the next row (seq %d): flushes at %v", w.Seq, first, j.flushes())
	}
	if finished < w.Seq {
		t.Errorf("the finished row (seq %d) precedes the cancel_waiting row (seq %d)", finished, w.Seq)
	}
	want := []any{
		map[string]any{"call_id": ids["crw.verify"], "op": "crw.verify", "node": "n1"},
		map[string]any{"call_id": ids["crw.integrate"], "op": "crw.integrate", "node": "n2"},
	}
	if ids["crw.integrate"] < ids["crw.verify"] {
		want[0], want[1] = want[1], want[0]
	}
	if got := w.Data["calls"]; !reflect.DeepEqual(normalizeCalls(got), want) {
		t.Errorf("cancel_waiting calls %#v, want %#v", got, want)
	}
	if w.RunID != "run-cw" || w.CallID != "" {
		t.Errorf("cancel_waiting row %+v", w)
	}
}

// flushJournal is a memory journal that batches like the SQLite one: it records how many rows it held at each Flush.
type flushJournal struct {
	*MemoryJournal
	mu sync.Mutex
	at []int
}

func (j *flushJournal) Flush() error {
	n := len(j.Events())
	j.mu.Lock()
	defer j.mu.Unlock()
	j.at = append(j.at, n)
	return nil
}

func (j *flushJournal) flushes() []int {
	j.mu.Lock()
	defer j.mu.Unlock()
	return append([]int(nil), j.at...)
}

// flushedBetween reports whether a Flush stored row seq before row next was appended.
func (j *flushJournal) flushedBetween(seq, next int) bool {
	for _, n := range j.flushes() {
		if n >= seq && n < next {
			return true
		}
	}
	return false
}

// normalizeCalls turns the row's calls into []any of map[string]any, whatever concrete slice type the engine wrote.
func normalizeCalls(v any) []any {
	rv := reflect.ValueOf(v)
	if rv.Kind() != reflect.Slice {
		return nil
	}
	out := make([]any, rv.Len())
	for i := range out {
		el := rv.Index(i).Interface()
		if m, ok := el.(map[string]any); ok {
			out[i] = m
			continue
		}
		out[i] = el
	}
	return out
}

// shape is what a row is, without its sequence and data.
func shape(events []Event) []string {
	out := make([]string, 0, len(events))
	for _, ev := range events {
		out = append(out, ev.Kind+"/"+ev.Event+"/"+ev.Op)
	}
	return out
}

// TestCancelWithNothingInFlightWritesNoWaitingRow: a run cancelled while only an agent call is in flight (an op completed before)
// writes no cancel_waiting row: its rows are B0's (the op's rows, the agent call's rows, the finished row).
func TestCancelWithNothingInFlightWritesNoWaitingRow(t *testing.T) {
	prog := loadCancelScript(t, "await crw.verify({ node: 'n1' })\nconst a = await agent.call('hi', { agentType: 'executor', node: 'n1' })\nreturn a.status\n")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	fake := NewFake()
	fake.Handler = func(r FakeRequest) FakeReply {
		if r.Kind == "turn" {
			cancel()
			time.Sleep(100 * time.Millisecond)
		}
		return FakeReply{}
	}
	j := NewMemoryJournal()
	res, err := Run(ctx, prog, Config{RunID: "run-cw0", Backend: fake, Policy: FakePolicy(), Journal: j, Ops: map[string]Op{"verify": quickOp{}}})
	if err != nil {
		t.Fatal(err)
	}
	if res.ErrorKind != ErrKindCancelled {
		t.Fatalf("the run did not end cancelled: %+v", res)
	}
	events := j.Events()
	if rows := waitingRows(events); len(rows) != 0 {
		t.Fatalf("cancel_waiting rows with nothing in flight: %+v", rows)
	}
	for _, s := range shape(events) {
		if s == KindFlow+"/"+EvCancelWaiting+"/" {
			t.Fatalf("rows %v", shape(events))
		}
	}
	last := events[len(events)-1]
	if last.Kind != KindRun || last.Event != "finished" {
		t.Fatalf("the last row is %s/%s, want run/finished: %v", last.Kind, last.Event, shape(events))
	}
}

// TestFailureOtherThanCancelWritesNoWaitingRow: a run that fails for another reason while an op is in flight (the script opens a
// second position of its path while crw.verify has not settled: path_concurrency, an abort that cancels the op, which outlives it)
// ends with that error and writes no cancel_waiting row; only a cancelled run says which ops it is ending.
func TestFailureOtherThanCancelWritesNoWaitingRow(t *testing.T) {
	prog := loadCancelScript(t, "crw.verify({ node: 'n1' })\ncrw.integrate('n2', 'head')\nreturn 1\n")
	started := make(chan string, 2)
	ops := map[string]Op{"verify": lingerOp{started, 300 * time.Millisecond}, "integrate": lingerOp{started, 300 * time.Millisecond}}
	j := NewMemoryJournal()
	res, err := Run(context.Background(), prog, Config{RunID: "run-cw1", Backend: NewFake(), Policy: FakePolicy(), Journal: j, Ops: ops})
	if err != nil {
		t.Fatal(err)
	}
	if res.ErrorKind != ErrKindPathConcurrency || res.Status != RunScriptError {
		t.Fatalf("the run did not end with path_concurrency: %+v", res)
	}
	events := j.Events()
	if rows := waitingRows(events); len(rows) != 0 {
		t.Fatalf("cancel_waiting rows on a failure other than a cancel: %+v", rows)
	}
	var refusedVerify bool
	for _, ev := range events {
		if ev.Kind == KindCall && ev.Op == "crw.verify" && ev.Event == EvRefused {
			refusedVerify = true
		}
	}
	if !refusedVerify {
		t.Fatalf("the op in flight left no refused row: %v", shape(events))
	}
}
