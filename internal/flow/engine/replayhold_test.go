package engine

import (
	"context"
	"encoding/json"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// stepOp answers its argument and counts its live runs and its proofs; every row it wrote is proven.
type stepOp struct {
	runs, proofs atomic.Int32
}

func (o *stepOp) Run(_ context.Context, req OpRequest) (OpResult, error) {
	o.runs.Add(1)
	var args []int
	_ = json.Unmarshal(req.Args, &args)
	b, _ := json.Marshal(map[string]int{"n": args[0]})
	return OpResult{Result: b}, nil
}

func (o *stepOp) Proven(context.Context, []Event, Event) (bool, string, error) {
	o.proofs.Add(1)
	return true, "", nil
}

// Config.ReplayHold, a test seam, holds a replayed op until the test settles it (CRW-1467): the script waits at that await - the
// next op is not reached, so it is not proven - and the run does not end; settle, called from another goroutine, resolves the op with
// its recorded result. The run then completes with the recorded values, runs no op live and writes no call row. A nil ReplayHold
// replays as before.
func TestReplayHoldSettlesAReplayedOpWhenReleased(t *testing.T) {
	const body = `const a = await crw.step(1)
const b = await crw.step(2)
return [a.n, b.n]`
	h := newHarness(t)
	op := &stepOp{}
	h.cfg.Ops = map[string]Op{"step": op}
	if got := value[[]int](t, h.run(body)); len(got) != 2 || got[0] != 1 || got[1] != 2 {
		t.Fatalf("the first run answered %v", got)
	}
	if op.runs.Load() != 2 {
		t.Fatalf("%d live runs in the first run, want 2", op.runs.Load())
	}
	callRows := func() int {
		n := 0
		for _, ev := range h.journal.Events() {
			if ev.Kind == KindCall {
				n++
			}
		}
		return n
	}
	rowsBefore := callRows()

	// the resume holds the first replayed row
	op.runs.Store(0)
	op.proofs.Store(0)
	var (
		mu     sync.Mutex
		held   []Event
		settle func()
	)
	heldOne := make(chan struct{})
	h.cfg.Resume = &ResumeOptions{}
	h.cfg.ReplayHold = func(row Event, s func()) bool {
		mu.Lock()
		defer mu.Unlock()
		held = append(held, row)
		if len(held) > 1 {
			return false
		}
		settle = s
		close(heldOne)
		return true
	}
	done := make(chan *Result, 1)
	errs := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		res, err := Run(ctx, h.load(body), h.cfg)
		if err != nil {
			errs <- err
			return
		}
		done <- res
	}()
	select {
	case <-heldOne:
	case err := <-errs:
		t.Fatal(err)
	case <-time.After(10 * time.Second):
		t.Fatal("the first replayed row was never handed to ReplayHold")
	}
	select {
	case res := <-done:
		t.Fatalf("the run ended while its first op was held: %s %s", res.Status, res.Error)
	case <-time.After(200 * time.Millisecond):
	}
	mu.Lock()
	if len(held) != 1 || held[0].Op != "crw.step" || rowStr(held[0].Data, "result") != `{"n":1}` {
		t.Errorf("held rows %+v, want the first crw.step row only", held)
	}
	if n := op.proofs.Load(); n != 1 {
		t.Errorf("%d proofs while the first op was held: the second op was reached", n)
	}
	s := settle
	mu.Unlock()
	go s()
	go s() // settle runs once
	var res *Result
	select {
	case res = <-done:
	case err := <-errs:
		t.Fatal(err)
	case <-time.After(10 * time.Second):
		t.Fatal("the run did not end after the held op was settled")
	}
	if got := value[[]int](t, res); len(got) != 2 || got[0] != 1 || got[1] != 2 {
		t.Errorf("the resume answered %v, want the recorded [1 2]", got)
	}
	if n := op.runs.Load(); n != 0 {
		t.Errorf("%d live op runs in the resume, want none", n)
	}
	mu.Lock()
	if len(held) != 2 {
		t.Errorf("ReplayHold saw %d rows, want both replayed rows", len(held))
	}
	mu.Unlock()
	if n := callRows(); n != rowsBefore {
		t.Errorf("the resume wrote %d call rows", n-rowsBefore)
	}

	// without the seam the resume replays as before
	h.cfg.ReplayHold = nil
	op.proofs.Store(0)
	if got := value[[]int](t, h.run(body)); len(got) != 2 || got[0] != 1 || got[1] != 2 {
		t.Errorf("a resume without ReplayHold answered %v", got)
	}
	if op.runs.Load() != 0 || op.proofs.Load() != 2 {
		t.Errorf("a resume without ReplayHold ran %d ops live and proved %d", op.runs.Load(), op.proofs.Load())
	}
	if n := callRows(); n != rowsBefore {
		t.Errorf("a resume without ReplayHold wrote %d call rows", n-rowsBefore)
	}
}
