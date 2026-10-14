package engine

import (
	"context"
	"testing"
	"time"
)

// CRW-1372 (1): the message id of a wake is permanent in the relay, and the re-notification count starts again at 0 in every run
// process, so the wake id carries the process: the n-th process of a run (its n-th started row) wakes under <batch>/e<n>, and a resume
// stages new messages instead of answers that it staged them already. A wake the relay staged nothing for is recorded staged:false with
// the reason, not staged:true.

// runUntilWoken runs the body until the first wake, then lets the owner take the step.
func runUntilWoken(t *testing.T, h *harness, orch *fakeOrch, rec *wakeRecorder) []SessionWake {
	t.Helper()
	body := `const a = await agent.call('look', { agentType: 'explorer', label: 'a' })
return a.status`
	done := make(chan *Result, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		res, err := Run(ctx, h.load(body), h.cfg)
		if err != nil {
			t.Error(err)
		}
		done <- res
	}()
	deadline := time.Now().Add(10 * time.Second)
	for {
		if wakes, _ := rec.got(); len(wakes) > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("no wake")
		}
		time.Sleep(5 * time.Millisecond)
	}
	orch.start(t)
	<-done
	wakes, _ := rec.got()
	return wakes
}

func TestSessionWakeIDCarriesTheRunProcess(t *testing.T) {
	h, orch, rec := wakeKit(t, "idle")
	h.cfg.Session.UnresponsiveAfter = time.Hour
	// an earlier process of the run: its started row is in the journal
	h.journal.Append(Event{RunID: "run-test", Kind: KindRun, Event: "started", Data: map[string]any{"workflow": "t", "backend": BackendSession}})
	wakes := runUntilWoken(t, h, orch, rec)
	if first := wakes[0]; first.ID != first.Batch+"/e2" {
		t.Fatalf("the second process's first wake is %q, want %q", first.ID, first.Batch+"/e2")
	}
	if got := wakeID(2, "b1", 3); got != "b1/e2/r3" {
		t.Errorf("wakeID(2, b1, 3) = %q", got)
	}
	if got := wakeID(1, "b1", 0); got != "b1" {
		t.Errorf("the first process keeps the id it had: %q", got)
	}
}

func TestSessionWakeThatStagedNothingIsRecordedAsSuch(t *testing.T) {
	h, orch, rec := wakeKit(t, "idle")
	h.cfg.Session.UnresponsiveAfter = time.Hour
	h.cfg.Session.Wake = func(ctx context.Context, w SessionWake) error {
		_ = rec.wake(ctx, w)
		return WakeNotStaged{Reason: "this message is already staged; one report is one message"}
	}
	runUntilWoken(t, h, orch, rec)
	rows := flowRows(h.journal, EvSessionWake)
	if len(rows) == 0 || rows[0].Data["staged"] != false || rowStr(rows[0].Data, "reason") != "this message is already staged; one report is one message" || rows[0].Data["error"] != nil {
		t.Fatalf("session_wake rows %+v", rows)
	}
}

// CRW-1372 (2): a session run pins its role pairs in its started row: its process binds the owner's line with them, and `crw flow next`
// reads them.
func TestSessionRunPinsItsRolePairsInItsStartedRow(t *testing.T) {
	h, orch, rec := wakeKit(t, "idle")
	h.cfg.Session.UnresponsiveAfter = time.Hour
	runUntilWoken(t, h, orch, rec)
	var pinned map[string]any
	for _, ev := range h.journal.Events() {
		if ev.Kind == KindRun && ev.Event == "started" {
			pinned, _ = ev.Data["role_pairs"].(map[string]any)
		}
	}
	explorer, _ := pinned["explorer"].(map[string]any)
	want := h.cfg.Policy.Roles["explorer"].Pair
	if explorer == nil || explorer["model"] != want.Model || explorer["effort"] != want.Effort {
		t.Fatalf("the started row pinned %v, want the explorer's %+v", pinned, want)
	}
}

// CRW-1372 (2), Argon V5 G1/G2 of fbed54e9: the run's process writes the owner's line from its own record. Started is called once, after
// the started row (which names the process) and the policy_repinned row of a resume that repinned are in the journal and before any row
// of a call, with the table the started row pinned.
func TestSessionRunCallsStartedAfterItsStartedRow(t *testing.T) {
	h, orch, rec := wakeKit(t, "idle")
	h.cfg.Session.UnresponsiveAfter = time.Hour
	// an earlier process pinned another digest: this one repins
	h.journal.Append(Event{RunID: "run-test", Kind: KindRun, Event: "started", Data: map[string]any{"workflow": "t", "backend": BackendSession, "policy_digest": "sha256:earlier"}})
	h.cfg.Session.Process = "0123456789abcdef"
	var calls int
	var at []Event
	var got *Policy
	h.cfg.Session.Started = func(table *Policy) {
		calls++
		at, got = h.journal.Events(), table
	}
	runUntilWoken(t, h, orch, rec)
	if calls != 1 || got != h.cfg.Policy {
		t.Fatalf("Started was called %d times with %p, want once with the run's table %p", calls, got, h.cfg.Policy)
	}
	last := at[len(at)-1]
	started := at[len(at)-2]
	if started.Kind != KindRun || started.Event != "started" || rowStr(started.Data, "process") != "0123456789abcdef" || started.Data["role_pairs"] == nil {
		t.Fatalf("the row before the repin when Started ran is %+v, want this process's started row naming it", started)
	}
	if last.Kind != KindRun || last.Event != EvPolicyRepinned {
		t.Fatalf("the last row when Started ran is %+v, want the policy_repinned row", last)
	}
	for _, ev := range at {
		if ev.Kind == KindCall {
			t.Fatalf("a call row was written before Started: %+v", ev)
		}
	}
}
