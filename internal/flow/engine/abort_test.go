package engine

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/flow/script"
)

// runBounded runs the body like h.run but fails the test when Run does not return within d: a run that ends must end
// even when something it waits on never settles.
func (h *harness) runBounded(body string, d time.Duration) *Result {
	h.t.Helper()
	prog := h.load(body)
	type out struct {
		res *Result
		err error
	}
	done := make(chan out, 1)
	go func() {
		res, err := Run(context.Background(), prog, h.cfg)
		done <- out{res, err}
	}()
	select {
	case o := <-done:
		if o.err != nil {
			h.t.Fatal(o.err)
		}
		return o.res
	case <-time.After(d):
		h.t.Fatalf("Run did not return within %s", d)
		return nil
	}
}

// assertEndsCleanly checks that the run's last journal row is its finished record and that every call the run opened
// has a terminal row before it.
func assertEndsCleanly(t *testing.T, h *harness) {
	t.Helper()
	ev := h.journal.Events()
	if len(ev) == 0 || ev[len(ev)-1].Kind != KindRun || ev[len(ev)-1].Event != "finished" {
		t.Fatalf("the finished record is not the last row: %+v", ev[len(ev)-1])
	}
	open := map[string]bool{}
	for _, e := range ev {
		if e.Kind != KindCall {
			continue
		}
		if e.Event == EvPrepared {
			open[e.CallID] = true
		}
		if _, ok := terminalEvents[e.Event]; ok {
			delete(open, e.CallID)
		}
	}
	if len(open) != 0 {
		t.Errorf("calls without a terminal row: %v", open)
	}
}

// A child workflow that hits a limit after it awaited a call ends the run: the parent's pending workflow() does not
// keep the run waiting.
func TestChildWorkflowLimitAfterAwaitEndsRun(t *testing.T) {
	for name, c := range map[string]struct {
		child  string
		limits script.Limits
		kind   string
	}{
		"step": {"await agent('x', { agentType: 'explorer' }); while (true) {}", script.Limits{Step: time.Second}, ErrKindStepLimit},
		"heap": {"await agent('x', { agentType: 'explorer' }); const a = []; while (true) a.push(new Array(4096).fill(1))", script.Limits{Step: 20 * time.Second, HeapBytes: 32 << 20}, ErrKindHeapLimit},
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			h.cfg.Limits = c.limits
			child, err := script.Load(script.Source{Name: "child.js", Text: []byte("export const meta = { name: 'child-loop' }\n" + c.child + "\n")})
			if err != nil {
				t.Fatal(err)
			}
			h.cfg.Workflows = func(name, path string) (*script.Program, error) { return child, nil }
			res := h.runBounded(`return await workflow('child-loop')`, 15*time.Second)
			if res.Status != RunScriptError || res.ErrorKind != c.kind {
				t.Errorf("%s %s: %s", res.Status, res.ErrorKind, res.Error)
			}
			assertEndsCleanly(t, h)
		})
	}
}

// After a fatal path_concurrency error the script runs no further: catching the error and calling again opens no new
// position, no intent row and no request, and the run ends at once.
func TestPathConcurrencyCaughtThenRetryEndsRun(t *testing.T) {
	h := newHarness(t)
	h.fake.Handler = func(r FakeRequest) FakeReply {
		time.Sleep(20 * time.Millisecond)
		return FakeReply{}
	}
	res := h.runBounded(`const p = agent.call('a', { agentType: 'explorer', label: 'a' })
try { agent.call('b', { agentType: 'explorer', label: 'b' }) } catch (e) { }
await p
for (;;) { await agent.call('c', { agentType: 'explorer', label: 'c' }) }`, 8*time.Second)
	if res.Status != RunScriptError || res.ErrorKind != ErrKindPathConcurrency {
		t.Fatalf("%s %s: %s", res.Status, res.ErrorKind, res.Error)
	}
	if rows := h.callRows("c"); len(rows) != 0 {
		t.Errorf("c has %d rows after the fatal error", len(rows))
	}
	if rows := h.callRows("b"); len(rows) != 0 {
		t.Errorf("b has %d rows", len(rows))
	}
	for _, r := range h.fake.Requests() {
		if r.Prompt == "c" || r.Prompt == "b" {
			t.Errorf("request after the fatal error: %+v", r)
		}
	}
	if st := terminalOf(h.callRows("a")); st == "" {
		t.Errorf("a has no terminal row")
	}
	// the call the abort cut off is reported with the result it ended on, though the script never received it
	if len(res.Calls) != 1 || res.Calls[0].Label != "a" || res.Calls[0].Status == "" {
		t.Errorf("calls %+v", res.Calls)
	}
	assertEndsCleanly(t, h)
	// a synchronous retry loop after the caught error ends the same way, without waiting for the step limit
	h = newHarness(t)
	start := time.Now()
	res = h.runBounded(`const p = agent.call('a', { agentType: 'explorer', label: 'a' })
for (;;) { try { agent.call('c', { agentType: 'explorer', label: 'c' }) } catch (e) { } }`, 8*time.Second)
	if res.Status != RunScriptError || res.ErrorKind != ErrKindPathConcurrency || time.Since(start) > 5*time.Second {
		t.Errorf("%s %s after %s: %s", res.Status, res.ErrorKind, time.Since(start), res.Error)
	}
	assertEndsCleanly(t, h)
}

// The final value is serialised under the step and heap limits: a toJSON or getter that loops or allocates without
// bound ends the run with the limit, and one that throws is the script's error.
func TestFinalValueSerialisationIsLimited(t *testing.T) {
	for name, c := range map[string]struct {
		body   string
		limits script.Limits
		kind   string
		msg    string
	}{
		"toJSON loops":     {"return { toJSON() { while (true) {} } }", script.Limits{Step: time.Second}, ErrKindStepLimit, ""},
		"getter loops":     {"return { get x() { while (true) {} } }", script.Limits{Step: time.Second}, ErrKindStepLimit, ""},
		"toJSON allocates": {"return { toJSON() { const a = []; while (true) a.push(new Array(4096).fill(1)) } }", script.Limits{Step: 20 * time.Second, HeapBytes: 32 << 20}, ErrKindHeapLimit, ""},
		"toJSON throws":    {"return { toJSON() { throw new Error('no json') } }", script.Limits{}, ErrKindJS, "no json"},
		"error text loops": {"throw { toString() { while (true) {} } }", script.Limits{Step: time.Second}, ErrKindStepLimit, ""},
		"cyclic return":    {"const o = {}; o.o = o; return o", script.Limits{}, ErrKindJS, "circular"},
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			h.cfg.Limits = c.limits
			res := h.runBounded(c.body, 25*time.Second)
			if res.Status != RunScriptError || res.ErrorKind != c.kind || !strings.Contains(strings.ToLower(res.Error), c.msg) {
				t.Errorf("%s %s: %s", res.Status, res.ErrorKind, res.Error)
			}
		})
	}
}
