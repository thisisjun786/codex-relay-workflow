package cli

import (
	"fmt"
	"os"
	"strings"
	"syscall"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/flow/engine"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/ops"
)

// A cancel whose run process does not end within --wait says what the run is ending (CRW-1573, D3.4): the ops of the run's latest
// cancel_waiting row and the agent calls whose last row is a start row, and that the run gives up its executions itself when they end.

// cancelHeldRun cancels run with a fake signal while a test holder keeps the run lock past --wait 1; it answers the exit status, the
// stderr and the pids signalled.
func cancelHeldRun(t *testing.T, w *world, run string) (int, string, []int) {
	t.Helper()
	td := w.deps()
	var sent []int
	td.Signal = func(pid int, sig syscall.Signal) error {
		if sig != syscall.SIGTERM {
			t.Errorf("signal %v, want SIGTERM", sig)
		}
		sent = append(sent, pid)
		return nil
	}
	held := holdLock(t, w, run, ProcessCommand)
	defer held.Release()
	code, _, errOut := td.do("cancel", run, "--state", w.state, "--wait", "1")
	return code, errOut, sent
}

// TestCancelWaitNamesTheCallsInFlight: the run wrote a cancel_waiting row naming its crw.verify of node A, and an agent call of node B
// has a turn_started row and no terminal row; a completed agent call and a completed op are not named.
func TestCancelWaitNamesTheCallsInFlight(t *testing.T) {
	w := newWorld(t)
	r := w.newRun("run-wait", "thread-owner")
	r.completed("c-done", "architect", "A", "thread-arch", 10)
	r.op("c-rel-a", ops.OpRelease, map[string]any{"plan": "plan-1", "node": "A", "released": true, "execution_id": "ex-a"})
	r.call("c-exec-b", "executor", "B", 1, []string{engine.EvPrepared, engine.EvThreadRequested, engine.EvThreadStarted, engine.EvTurnRequested, engine.EvTurnStarted},
		map[string]map[string]any{engine.EvThreadStarted: {"thread_id": "thread-b"}})
	r.flow(engine.EvCancelWaiting, map[string]any{"calls": []any{
		map[string]any{"call_id": "c-clock", "op": "crw.clock", "node": ""},
		map[string]any{"call_id": "c-verify-a", "op": ops.OpVerify, "node": "A"},
	}})
	r.close()
	code, errOut, sent := cancelHeldRun(t, w, "run-wait")
	if code != 1 || len(sent) != 1 || sent[0] != os.Getpid() {
		t.Fatalf("cancel: exit %d, signals to %v, err %q", code, sent, errOut)
	}
	for _, want := range []string{
		fmt.Sprintf("crw flow cancel: process %d still holds run run-wait after 1s", os.Getpid()),
		"it is ending crw.clock (call c-clock), " + ops.OpVerify + " (call c-verify-a, node A), executor:B (call c-exec-b)",
		"when they end the run gives up its executions itself (no second cancel)",
	} {
		if !strings.Contains(errOut, want) {
			t.Errorf("stderr has no %q: %q", want, errOut)
		}
	}
	for _, not := range []string{"c-done", "c-rel-a", "no call in flight is known"} {
		if strings.Contains(errOut, not) {
			t.Errorf("stderr names %q: %q", not, errOut)
		}
	}
}

// TestCancelWaitWithNoKnownCall: a run with no cancel_waiting row and no agent call running says no call in flight is known.
func TestCancelWaitWithNoKnownCall(t *testing.T) {
	w := newWorld(t)
	r := w.newRun("run-quiet", "thread-owner")
	r.completed("c-done", "architect", "A", "thread-arch", 10)
	r.close()
	code, errOut, sent := cancelHeldRun(t, w, "run-quiet")
	if code != 1 || len(sent) != 1 {
		t.Fatalf("cancel: exit %d, signals to %v, err %q", code, sent, errOut)
	}
	for _, want := range []string{"still holds run run-quiet after 1s", "no call in flight is known", "when they end the run gives up its executions itself (no second cancel)"} {
		if !strings.Contains(errOut, want) {
			t.Errorf("stderr has no %q: %q", want, errOut)
		}
	}
	if strings.Contains(errOut, "c-done") {
		t.Errorf("stderr names the completed call: %q", errOut)
	}
}

// TestInflightCallsTakesTheLatestCancelWaitingRow: of two cancel_waiting rows the latest one names the ops, and an agent call that
// ended after its start is not named.
func TestInflightCallsTakesTheLatestCancelWaitingRow(t *testing.T) {
	calls := func(ids ...string) map[string]any {
		var list []any
		for _, id := range ids {
			list = append(list, map[string]any{"call_id": id, "op": ops.OpVerify, "node": "N"})
		}
		return map[string]any{"calls": list}
	}
	events := []engine.Event{
		{Kind: engine.KindFlow, Event: engine.EvCancelWaiting, Data: calls("c-old")},
		{Kind: engine.KindCall, CallID: "c-a", Op: "agent", Label: "critic:N", Event: engine.EvTurnStarted},
		{Kind: engine.KindCall, CallID: "c-a", Op: "agent", Label: "critic:N", Event: engine.EvCompleted},
		{Kind: engine.KindFlow, Event: engine.EvCancelWaiting, Data: calls("c-new")},
	}
	got := strings.Join(inflightCalls(events), "; ")
	if want := ops.OpVerify + " (call c-new, node N)"; got != want {
		t.Errorf("inflightCalls %q, want %q", got, want)
	}
}
