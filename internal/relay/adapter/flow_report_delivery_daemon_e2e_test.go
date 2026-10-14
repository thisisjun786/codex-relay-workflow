package adapter

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/appserver/fakehost"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/report"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/daemon"
)

// CRW-1390 (the D2 follow-ups d4 and the held_uncertain record, CRW-1380): the delivery rows of a report over the supervisor channel,
// through the daemon's own tick over the scripted App Server.
//
//   - d4: a hold that is released and taken again under the same cap is the same result of the same report: the settle's guard (a row
//     once for its key and result) writes no second refused row; the attempt cap (an unloaded thread whose resume fails before anything
//     is sent, six times) is a refused row with the reason attempt_cap, as the busy cap is.
//   - a send whose outcome is unknown (the daemon died after the transport and before its settle) is recorded unknown once the next
//     daemon finds it stranded, and a replay of the report stages nothing and writes no queued row over it.

// busyThread answers every thread/read as an active turn.
func (w *mgmtWorld) busyThread() {
	w.host.Handle("thread/read", func(json.RawMessage) fakehost.Reply {
		return fakehost.Reply{Result: map[string]any{"thread": map[string]any{"status": map[string]any{"type": "active", "activeFlags": []any{}}, "canAcceptDirectInput": true}}}
	})
}

// tickUntilHeld ticks, the clock moving past every backoff, until the message is held at hold.
func (f *flowWorld) tickUntilHeld(message, hold string) {
	f.t.Helper()
	for i := 0; i < 200; i++ {
		f.tick(1)
		f.clock.T += 1000
		row, err := f.s.SupervisorMessage(f.ctx, message)
		if err != nil {
			f.t.Fatal(err)
		}
		if row.HoldReason.Valid && row.HoldReason.String == hold {
			return
		}
	}
	f.t.Fatalf("the message never reached %s\n%s", hold, f.dump())
}

// refusedRows are the refused report_delivery rows of the report, by reason.
func (f *flowWorld) refusedRows(id string) map[string]int {
	f.t.Helper()
	rows, err := report.ReadRows(f.ctx, f.s.DB, flowRun, report.KindDelivery)
	if err != nil {
		f.t.Fatal(err)
	}
	out := map[string]int{}
	for _, r := range rows {
		if r.Str("report_id") == id && r.Str("result") == "refused" {
			out[r.Str("reason")]++
		}
	}
	return out
}

// dyingTick runs one daemon tick that dies (its context ends) right after the host started the turn, before the settle commits.
func dyingTick(t *testing.T, w *mgmtWorld) {
	t.Helper()
	dying, die := context.WithCancel(w.ctx)
	defer die()
	started := false
	w.host.Handle("turn/start", func(json.RawMessage) fakehost.Reply {
		started = true
		die()
		return fakehost.Reply{Result: map[string]any{"turn": map[string]any{"id": "turn-dying"}}}
	})
	_, _ = w.d.Tick(dying)
	if !started {
		t.Fatalf("the transport never started\n%s", w.dump())
	}
}

func TestFlowReportE2EABusyCapHeldAgainAfterItsReleaseWritesNoSecondRefusedRow(t *testing.T) {
	w := newMgmtWorld(t)
	w.relay("linkage-bind", "--role", "supervisor", "--scope-kind", "management", "--scope", mgmtA, "--task", mgmtA, "--host", "host")
	w.bindOwner("P-A", ownerA, mgmtA)
	w.busyThread()
	w.open(mgmtA)
	f := w.flowRun("P-A")
	res, err := f.writer().Up(w.ctx, blockedReport())
	if err != nil {
		t.Fatal(err)
	}
	message, _ := f.flowMessage("flow_project_blocked")
	f.tickUntilHeld(message, "busy_cap")
	if got := f.refusedRows(res.ReportID); got["busy_cap"] != 1 || len(got) != 1 {
		t.Fatalf("the first busy cap wrote the refused rows %v\n%s", got, w.dump())
	}
	// an edge of the thread releases the hold (D1b); the thread is busy again, and the next busy answer holds the message at the cap again
	woken, err := w.c.WakeBusyRecipient(w.ctx, mgmtA, "idle", w.clock.T)
	if err != nil || woken.Released != 1 {
		t.Fatalf("the edge released %+v (%v), want the one hold", woken, err)
	}
	if row, err := w.s.SupervisorMessage(w.ctx, message); err != nil || row.HoldReason.Valid {
		t.Fatalf("after the release the message is %+v (%v)", row, err)
	}
	f.tickUntilHeld(message, "busy_cap")
	if got := f.refusedRows(res.ReportID); got["busy_cap"] != 1 || len(got) != 1 {
		t.Fatalf("the second busy cap of the same report wrote the refused rows %v, want the one row\n%s", got, w.dump())
	}
	if got := f.deliveries(res.ReportID); got["queued"] != 1 || got["refused"] != 1 || len(got) != 2 {
		t.Fatalf("the deliveries are %v", got)
	}
	if len(w.turnsOf(mgmtA)) != 0 {
		t.Fatalf("a busy thread was given a turn: %q", w.turnsOf(mgmtA))
	}
	f.noParentQueue()
}

func TestFlowReportE2EAnAttemptCapHoldWritesTheRefusedDelivery(t *testing.T) {
	w := newMgmtWorld(t)
	w.relay("linkage-bind", "--role", "supervisor", "--scope-kind", "management", "--scope", mgmtA, "--task", mgmtA, "--host", "host")
	w.bindOwner("P-A", ownerA, mgmtA)
	// the management thread is not loaded and its resume fails before anything is sent: every attempt is withheld before the send
	w.host.Handle("thread/read", func(json.RawMessage) fakehost.Reply {
		return fakehost.Reply{Result: map[string]any{"thread": map[string]any{"status": map[string]any{"type": "notLoaded"}, "canAcceptDirectInput": true}}}
	})
	w.host.Respond("thread/resume", fakehost.Reply{Error: &fakehost.RPCError{Code: -32000, Message: "the thread cannot be resumed"}})
	w.open(mgmtA)
	f := w.flowRun("P-A")
	res, err := f.writer().Up(w.ctx, blockedReport())
	if err != nil {
		t.Fatal(err)
	}
	message, _ := f.flowMessage("flow_project_blocked")
	f.tickUntilHeld(message, "attempt_cap")
	if got := f.refusedRows(res.ReportID); got["attempt_cap"] != 1 || len(got) != 1 {
		t.Fatalf("the attempt cap wrote the refused rows %v, want one attempt_cap row\n%s", got, w.dump())
	}
	if len(w.turnsOf(mgmtA)) != 0 {
		t.Fatalf("a thread whose resume failed was given a turn: %q", w.turnsOf(mgmtA))
	}
	// released and capped again (the attempt count stands past the cap): the same result of the same report, no second row
	if woken, err := w.c.WakeBusyRecipient(w.ctx, mgmtA, "idle", w.clock.T); err != nil || woken.Released != 1 {
		t.Fatalf("the edge released %+v (%v)", woken, err)
	}
	f.tickUntilHeld(message, "attempt_cap")
	if got := f.refusedRows(res.ReportID); got["attempt_cap"] != 1 || len(got) != 1 {
		t.Fatalf("the second attempt cap wrote the refused rows %v, want the one row\n%s", got, w.dump())
	}
	views, err := report.ReportsMatching(w.ctx, w.s.DB, report.ReportFilter{Project: "P-A"})
	if err != nil || len(views) != 1 || !views[0].Standing || views[0].LastDelivery != "refused" {
		t.Fatalf("the report reads %+v (%v), want refused and standing", views, err)
	}
	f.noParentQueue()
}

func TestFlowReportE2EAStrandedSendIsRecordedUnknownAndAReplayWritesNoQueuedRowOverIt(t *testing.T) {
	w := newMgmtWorld(t)
	w.relay("linkage-bind", "--role", "supervisor", "--scope-kind", "management", "--scope", mgmtA, "--task", mgmtA, "--host", "host")
	w.bindOwner("P-A", ownerA, mgmtA)
	w.open(mgmtA)
	f := w.flowRun("P-A")
	id := f.milestone()
	dyingTick(t, w)
	if got := f.deliveries(id); got["queued"] != 1 || len(got) != 1 {
		t.Fatalf("a daemon that died before its settle left the deliveries %v", got)
	}
	// the next daemon finds the stranded send once its lease is over: held_uncertain, not repeated, and recorded unknown
	w.clock.T += 400
	w.d = daemon.New(w.s, w.a, w.clock, w.c)
	w.tick(3)
	message, state := f.flowMessage("flow_milestone_report")
	if state != "held_uncertain" {
		t.Fatalf("the stranded message is %s\n%s", state, w.dump())
	}
	if got := f.deliveries(id); got["queued"] != 1 || got["unknown"] != 1 || len(got) != 2 {
		t.Fatalf("after the restart the deliveries are %v, want the queued row and one unknown\n%s", got, w.dump())
	}
	rows, err := report.ReadRows(w.ctx, w.s.DB, flowRun, report.KindDelivery)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rows {
		if r.Str("result") == "unknown" && (r.Str("message_id") != message || r.Str("observation") != "held_uncertain" || r.Subject != flowRun+"/"+id) {
			t.Fatalf("the unknown row is %+v (subject %q)", r.Payload, r.Subject)
		}
	}
	// a replay (a resume of the run closing the milestone again) finds the message as it may have gone: nothing is staged or written
	if again := f.milestone(); again != id {
		t.Fatalf("the replay answered %q, want %q", again, id)
	}
	w.tick(2)
	if got := f.deliveries(id); got["queued"] != 1 || got["unknown"] != 1 || len(got) != 2 {
		t.Fatalf("after the replay the deliveries are %v, want them unchanged", got)
	}
	views, err := report.ReportsMatching(w.ctx, w.s.DB, report.ReportFilter{Project: "P-A"})
	if err != nil || len(views) != 1 || !views[0].Standing || views[0].LastDelivery != "unknown" {
		t.Fatalf("the report reads %+v (%v), want unknown and standing", views, err)
	}
	if len(w.turnsOf(mgmtA)) != 0 {
		t.Fatalf("the uncertain send was repeated: %q", w.turnsOf(mgmtA))
	}
	f.noParentQueue()
}
