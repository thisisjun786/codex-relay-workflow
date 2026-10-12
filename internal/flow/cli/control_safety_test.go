package cli

import (
	"context"
	"errors"
	"os"
	"strings"
	"syscall"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/flow/ops"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// TestControlRefusesAnUnknownLockHolder: a run whose lock is held but names no process (the record is empty or unreadable) is neither
// paused nor cancelled by a signal; a signal to pid 0 would go to the process group of the controller (CRW-1205 d1).
func TestControlRefusesAnUnknownLockHolder(t *testing.T) {
	for name, record := range map[string]string{"empty": "", "garbage": "not json\n", "pid zero": `{"pid":0}` + "\n", "pid one": `{"pid":1}` + "\n", "negative": `{"pid":-4}` + "\n"} {
		t.Run(name, func(t *testing.T) {
			w := newWorld(t)
			w.newRun("run-anon", "thread-owner").close()
			td := w.deps()
			var sent []int
			td.Signal = func(pid int, sig syscall.Signal) error { sent = append(sent, pid); return nil }
			if err := os.MkdirAll(RunDir(w.state, "run-anon"), 0o700); err != nil {
				t.Fatal(err)
			}
			f, err := os.OpenFile(lockPath(w.state, "run-anon"), os.O_CREATE|os.O_RDWR, 0o600)
			if err != nil {
				t.Fatal(err)
			}
			defer f.Close()
			if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
				t.Fatal(err)
			}
			if _, err := f.WriteString(record); err != nil {
				t.Fatal(err)
			}
			for _, verb := range []string{"pause", "cancel"} {
				code, out, errOut := td.do(verb, "run-anon", "--state", w.state, "--wait", "0")
				if code == 0 || len(sent) != 0 || !strings.Contains(errOut, "holder") {
					t.Fatalf("%s: exit %d, signals to %v, out %q, err %q", verb, code, sent, out, errOut)
				}
			}
		})
	}
}

// releasedRun is a run that released nodes A and B to executors and gave B up (a completed crw.escalate row that closed it).
func releasedRun(w *world, id string) {
	r := w.newRun(id, "thread-owner")
	r.op("c-rel-a", ops.OpRelease, map[string]any{"plan": "plan-1", "node": "A", "released": true, "execution_id": "ex-a"})
	r.op("c-rel-b", ops.OpRelease, map[string]any{"plan": "plan-1", "node": "B", "released": true, "execution_id": "ex-b"})
	r.op("c-esc-b", ops.OpEscalate, map[string]any{"node": "B", "reason": "tiers_exhausted", "closed": true})
	r.close()
}

// TestCancelGivesUpTheExecutionsOfTheRun: cancelling a run no process holds closes the executions it released and still owns (their
// slots go back) before the run is marked cancelled; a node the run gave up itself is not closed again, a failure leaves the run
// as it was so the cancel can be repeated, and a cancel whose process ended earlier still settles (CRW-1205 d7).
func TestCancelGivesUpTheExecutionsOfTheRun(t *testing.T) {
	w := newWorld(t)
	releasedRun(w, "run-held")
	td := w.deps()
	var settled [][]ops.ReleaseRef
	fail := true
	td.Settle = func(_ context.Context, _ *store.Store, runID string, nodes []ops.ReleaseRef) (Settlement, error) {
		if runID != "run-held" {
			t.Errorf("the settlement is for run %q", runID)
		}
		settled = append(settled, nodes)
		if fail {
			return Settlement{}, errors.New("the relay is busy")
		}
		return Settlement{Closed: len(nodes), SlotsReturned: len(nodes)}, nil
	}
	if code, _, errOut := td.do("cancel", "run-held", "--state", w.state); code == 0 || !strings.Contains(errOut, "the relay is busy") {
		t.Fatalf("a failed settlement: %d %q", code, errOut)
	}
	if _, out, _ := td.do("show", "run-held", "--state", w.state, "--json"); strings.Contains(out, `"state": "cancelled"`) {
		t.Fatalf("the run was marked cancelled with its executions open: %s", out)
	}
	fail = false
	code, out, errOut := td.do("cancel", "run-held", "--state", w.state)
	if code != 0 || !strings.Contains(out, "1 execution(s) given up, 1 slot(s) returned") {
		t.Fatalf("cancel: %d %q %q", code, out, errOut)
	}
	if len(settled) != 2 || len(settled[1]) != 1 || settled[1][0] != (ops.ReleaseRef{Plan: "plan-1", Node: "A", Execution: "ex-a"}) {
		t.Errorf("the nodes settled: %+v", settled)
	}
	if _, out, _ := td.do("show", "run-held", "--state", w.state, "--json"); !strings.Contains(out, `"state": "cancelled"`) {
		t.Errorf("show: %s", out)
	}
	// a cancel that finds the run cancelled settles again (idempotent) and says there is nothing to cancel
	settled = nil
	if code, _, errOut := td.do("cancel", "run-held", "--state", w.state); code != 1 || !strings.Contains(errOut, "nothing to cancel") || len(settled) != 1 {
		t.Errorf("a second cancel: %d %q, %d settlements", code, errOut, len(settled))
	}
}
