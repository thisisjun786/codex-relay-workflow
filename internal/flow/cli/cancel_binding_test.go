package cli

import (
	"context"
	"os"
	"strings"
	"syscall"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/flow/engine"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/journal"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/report"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/registry"
)

func TestCancelReleasesTheProjectBindingAndRecordsIt(t *testing.T) {
	l := newLaneLab(t, labIssue{Key: "A-1", Milestone: "M1"})
	run := l.start("start", "project-lane", "--args", `{"plan":"plan-1","milestone":"M1"}`, "--handoff", l.file)
	id, err := RunIdentity(ctx, l.w.st, run)
	if err != nil {
		t.Fatal(err)
	}
	j, err := journal.Open(ctx, l.w.st, run.RunID, id, journal.Options{Immediate: true})
	if err != nil {
		t.Fatal(err)
	}
	j.Append(engine.Event{RunID: run.RunID, Kind: engine.KindRun, Event: "started", Data: map[string]any{"workflow": "project-lane", "script_digest": "script", "args_digest": "args"}})
	if err := j.Close(); err != nil {
		t.Fatal(err)
	}
	code, out, errOut := l.td.do("cancel", run.RunID, "--state", l.w.state, "--socket", l.host.SocketPath)
	if code != 0 {
		t.Fatalf("cancel: %d %s %s", code, out, errOut)
	}
	var status string
	if err := l.w.st.DB.QueryRow("SELECT status FROM scope_bindings WHERE binding_id='bind-parent'").Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "cancelled" {
		t.Errorf("binding status=%s; want cancelled", status)
	}
	if n := l.count("SELECT COUNT(*) FROM journal WHERE kind='scope_released'"); n != 1 {
		t.Errorf("release records=%d; want 1", n)
	}
	// The same task binds again: an old cancellation may not take its new tenure.
	reg := &registry.Registry{Store: l.w.st}
	if _, err := reg.BindScope(context.Background(), "parent", "P", registry.Endpoint{TaskID: l.owner, HostID: "host"}); err != nil {
		t.Fatal(err)
	}
	l.td.do("cancel", run.RunID, "--state", l.w.state, "--socket", l.host.SocketPath)
	reg.Store = l.w.st
	if err := l.w.st.DB.QueryRow("SELECT status FROM scope_bindings WHERE binding_id='bind-parent'").Scan(&status); err != nil {
		t.Fatal(err)
	}
	// The lab's seed id differs from the registry's derived id; inspect the live owner instead.
	owners, err := reg.Owners(ctx, "project", "P")
	if err != nil || len(owners) != 1 {
		t.Fatalf("rebound owner: %v %v", owners, err)
	}
}

func bindingTestRun(t *testing.T, l *laneLab) Spec {
	t.Helper()
	run := l.start("start", "project-lane", "--args", `{"plan":"plan-1","milestone":"M1"}`, "--handoff", l.file)
	id, err := RunIdentity(ctx, l.w.st, run)
	if err != nil {
		t.Fatal(err)
	}
	j, err := journal.Open(ctx, l.w.st, run.RunID, id, journal.Options{Immediate: true})
	if err != nil {
		t.Fatal(err)
	}
	j.Append(engine.Event{RunID: run.RunID, Kind: engine.KindRun, Event: "started", Data: map[string]any{"workflow": "project-lane", "script_digest": "script", "args_digest": "args"}})
	if err := j.Close(); err != nil {
		t.Fatal(err)
	}
	return run
}

func TestOldCancelLeavesANewerRunBinding(t *testing.T) {
	l := newLaneLab(t, labIssue{Key: "A-1", Milestone: "M1"})
	old := bindingTestRun(t, l)
	// Starting another run pins the tenure before its process produces any row.
	l.start("start", "project-lane", "--args", `{"plan":"plan-1","milestone":"M1"}`, "--handoff", l.file)
	if code, out, errOut := l.td.do("cancel", old.RunID, "--state", l.w.state, "--socket", l.host.SocketPath); code != 0 {
		t.Fatalf("%d %s %s", code, out, errOut)
	}
	var state string
	if err := l.w.st.DB.QueryRow("SELECT status FROM scope_bindings WHERE binding_id='bind-parent'").Scan(&state); err != nil {
		t.Fatal(err)
	}
	if state != "active" || l.count("SELECT COUNT(*) FROM journal WHERE kind='scope_released'") != 0 {
		t.Fatalf("old cancel changed new owner: %s", state)
	}
}

func TestCancelKeepsTheBindingWhileAnotherRunIsUnfinished(t *testing.T) {
	l := newLaneLab(t, labIssue{Key: "A-1", Milestone: "M1"})
	bindingTestRun(t, l)
	current := bindingTestRun(t, l)
	code, _, errOut := l.td.do("cancel", current.RunID, "--state", l.w.state, "--socket", l.host.SocketPath)
	if code == 0 || !strings.Contains(errOut, "other unfinished runs") {
		t.Fatalf("cancel %d %s", code, errOut)
	}
	if n := l.count("SELECT COUNT(*) FROM scope_bindings WHERE status='active'"); n != 1 {
		t.Fatalf("live bindings %d", n)
	}
	if n := l.count("SELECT COUNT(*) FROM journal WHERE kind='scope_released'"); n != 0 {
		t.Fatalf("release rows %d", n)
	}
}

// The shipped report hook and cancellation settlement together, over a run on
// the scripted App Server. No transport is called during or after its stop.
type cancelNoticeCounter struct{ count int }

func (s *cancelNoticeCounter) SendParent(context.Context, report.ParentSend) (report.SendResult, error) {
	s.count++
	return report.SendResult{Class: "accepted"}, nil
}
func (s *cancelNoticeCounter) SendSupervisor(context.Context, report.SupervisorSend) (report.SendResult, error) {
	s.count++
	return report.SendResult{Class: "accepted"}, nil
}

func TestCancelFakeHostReleasesBindingWithoutWakingUpperLayer(t *testing.T) {
	l := newLaneLab(t, labIssue{Key: "A-1", Milestone: "M1"})
	l.outsideHead("A-1", "outside-a1")
	author := l.authorThread("claude-fake-sonnet", "high")
	sent := &cancelNoticeCounter{}
	inner := l.td.World
	l.td.World = func(ctx context.Context, req WorldRequest) (World, error) {
		w, err := inner(ctx, req)
		if err != nil {
			return w, err
		}
		st, _, err := OpenWriter(ctx, req.State, req.Spec.Socket)
		if err != nil {
			return w, err
		}
		closeWorld := w.Close
		w.Close = func() error {
			defer st.Close()
			if closeWorld != nil {
				return closeWorld()
			}
			return nil
		}
		w.Upward = &report.RunHook{Store: st, Journal: w.Journal, Rows: report.ForeignAppender{Store: st, Run: req.Spec.RunID}, Sender: sent, Run: req.Spec.RunID}
		return w, nil
	}
	b := blockVerify(t)
	run := l.start("start", "land-head", "--args", landArgs(l, "A-1", "outside-a1", author), "--handoff", l.file)
	sigs := make(chan os.Signal, 1)
	go func() { <-b.reached; sigs <- syscall.SIGTERM }()
	code, out, errOut := l.processSigs(run.RunID, sigs)
	if code != 1 {
		t.Fatalf("cancel exit %d %s %s", code, out, errOut)
	}
	if sent.count != 0 {
		t.Fatalf("upper sends=%d", sent.count)
	}
	if n := l.count("SELECT COUNT(*) FROM scope_bindings WHERE binding_id='bind-parent' AND status='cancelled'"); n != 1 {
		t.Fatalf("binding not released: %s %s", out, errOut)
	}
	if n := l.count("SELECT COUNT(*) FROM journal WHERE kind='scope_released'"); n != 1 {
		t.Fatalf("release rows=%d", n)
	}
}
