package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/flow/apply"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/engine"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/journal"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/report"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

type fakeSender struct {
	supervisor []report.SupervisorSend
	reply      report.SendResult
}

func (f *fakeSender) SendSupervisor(_ context.Context, s report.SupervisorSend) (report.SendResult, error) {
	f.supervisor = append(f.supervisor, s)
	return f.reply, nil
}

func (f *fakeSender) SendParent(context.Context, report.ParentSend) (report.SendResult, error) {
	return report.SendResult{Class: "queued"}, nil
}

type fixedResolver struct{ rec report.Recipient }

func (f fixedResolver) Recipient(context.Context, string, string) (report.Recipient, error) {
	return f.rec, nil
}

// cliWorld is a store holding a run, and the environment of the commands over it.
type cliWorld struct {
	t      *testing.T
	st     *store.Store
	env    Environment
	out    *bytes.Buffer
	errOut *bytes.Buffer
	sender *fakeSender
}

func newCLIWorld(t *testing.T) *cliWorld {
	t.Helper()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "relay.sqlite3"), "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	j, err := journal.Open(ctx, st, "run-1", journal.Identity{PlanID: "plan-1", PlanRevision: 1, ActorTaskID: "orch-task", EpochNonce: "n"}, journal.Options{Now: func() time.Time { return time.Date(2026, 10, 12, 9, 0, 0, 0, time.UTC) }})
	if err != nil {
		t.Fatal(err)
	}
	j.Append(engine.Event{RunID: "run-1", Kind: engine.KindRun, Event: "started", Data: map[string]any{"workflow": "project-lane", "script_digest": "d", "args_digest": "a"}})
	if err := j.Flush(); err != nil {
		t.Fatal(err)
	}
	w := &cliWorld{t: t, st: st, out: &bytes.Buffer{}, errOut: &bytes.Buffer{}, sender: &fakeSender{reply: report.SendResult{Class: "accepted", RequestID: "req-1"}}}
	w.env = Environment{Stdout: w.out, Stderr: w.errOut, Sender: w.sender, Resolver: fixedResolver{report.Recipient{State: report.RecipientInitiative, Thread: "lead-1", InitiativeKey: "INI-1", ProjectKey: "PRJ-1"}},
		Open: func(context.Context, string, string) (*store.Store, string, error) {
			// a command closes the store it opened; the test keeps its own handle open
			again, err := store.Open(ctx, st.Path, "")
			return again, filepath.Dir(st.Path), err
		}}
	return w
}

func (w *cliWorld) run(args ...string) int {
	w.t.Helper()
	w.out.Reset()
	w.errOut.Reset()
	return RunEnv(ctx, args, w.env)
}

func (w *cliWorld) json(v any) {
	w.t.Helper()
	if err := json.Unmarshal(w.out.Bytes(), v); err != nil {
		w.t.Fatalf("stdout is no JSON: %v\n%s", err, w.out.String())
	}
}

func TestHandlesTheReachUpCommands(t *testing.T) {
	for _, c := range []string{"apply", "report-up", "reports", "report-ack", "report-delivered"} {
		if !Handles([]string{c}) {
			t.Errorf("%s is not handled", c)
		}
	}
	// resume and recover are the run surface's too (under the run lock, cli.go); the engine keeps run, lint, roles and compile
	for _, c := range []string{"run", "lint", "roles", "compile", "export", ""} {
		if Handles([]string{c}) {
			t.Errorf("%s is handled", c)
		}
	}
	if Handles(nil) {
		t.Error("no command is handled")
	}
}

// The orchestrator lifts a project-level blocker to the initiative lead: the evidence is the body, the report row is written and the lead's
// thread is sent the notice; reports lists it unread until the lead acknowledges it in a turn, and the pump's delivery is a row a flow
// command writes.
func TestReportUpReportsAckAndDeliveredCommands(t *testing.T) {
	w := newCLIWorld(t)
	evidence := filepath.Join(t.TempDir(), "evidence.md")
	if err := os.WriteFile(evidence, []byte("the plan lacks the schema decision"), 0o600); err != nil {
		t.Fatal(err)
	}
	if code := w.run("report-up", "--kind", "project_blocked", "--run", "run-1", "--evidence", evidence, "--subject", "schema decision"); code != ExitOK {
		t.Fatalf("report-up: exit %d %s", code, w.errOut.String())
	}
	var up report.UpResult
	w.json(&up)
	if up.Kind != "project_blocked" || up.Delivery != report.ResultAccepted || up.Recipient.Thread != "lead-1" || up.ReportID == "" || up.Standing {
		t.Fatalf("result %+v", up)
	}
	if len(w.sender.supervisor) != 1 || !strings.Contains(w.sender.supervisor[0].Text, "the plan lacks the schema decision") || w.sender.supervisor[0].ReportID != up.ReportID {
		t.Fatalf("sent %+v", w.sender.supervisor)
	}
	// unread
	if code := w.run("reports", "--initiative", "INI-1", "--unread", "--json"); code != ExitOK {
		t.Fatalf("reports: exit %d", code)
	}
	var views []report.ReportView
	w.json(&views)
	if len(views) != 1 || views[0].ID != up.ReportID || views[0].Read || views[0].LastDelivery != report.ResultAccepted {
		t.Fatalf("views %+v", views)
	}
	if code := w.run("reports", "--unread"); code != ExitOK || !strings.Contains(w.out.String(), up.ReportID) || !strings.Contains(w.out.String(), "unread") {
		t.Fatalf("reports table: exit %d\n%s", code, w.out.String())
	}
	// acknowledged
	if code := w.run("report-ack", up.ReportID, "--turn", "turn-lead-3"); code != ExitOK {
		t.Fatalf("report-ack: exit %d %s", code, w.errOut.String())
	}
	if code := w.run("reports", "--initiative", "INI-1", "--unread", "--json"); code != ExitOK {
		t.Fatal(code)
	}
	w.json(&views)
	if len(views) != 0 {
		t.Fatalf("unread after the ack: %+v", views)
	}
	// the pump's delivery of a queued report
	if code := w.run("report-delivered", "--report", up.ReportID, "--result", "accepted", "--request-id", "req-pump"); code != ExitOK {
		t.Fatalf("report-delivered: exit %d %s", code, w.errOut.String())
	}
	rows, err := report.ReadRows(ctx, w.st.DB, "run-1", report.KindReport, report.KindDelivery, report.KindRead)
	if err != nil {
		t.Fatal(err)
	}
	var kinds []string
	for _, r := range rows {
		kinds = append(kinds, r.Kind)
	}
	if strings.Join(kinds, ",") != "report,report_delivery,report_read,report_delivery" {
		t.Errorf("rows %v", kinds)
	}
	// a refused delivery is exit 1 with the report on record
	w.sender.reply = report.SendResult{Class: "refused", RequestID: "req-2"}
	if code := w.run("report-up", "--kind", "project_decision_request", "--run", "run-1", "--evidence", evidence, "--subject", "which schema"); code != ExitRefused {
		t.Errorf("a refused delivery: exit %d", code)
	}
}

func TestReachUpCommandsRefuseBadLines(t *testing.T) {
	w := newCLIWorld(t)
	for _, args := range [][]string{
		{"report-up", "--run", "run-1"},
		{"report-up", "--kind", "project_blocked", "--run", "run-1", "--evidence", "/nonexistent/evidence"},
		{"report-up", "--kind", "banana", "--run", "run-1", "--evidence", os.Args[0]},
		{"report-ack", "--turn", "t"},
		{"report-ack", "rep_x"},
		{"report-delivered", "--report", "rep_x"},
		{"report-delivered", "--report", "rep_missing", "--result", "accepted"},
		{"reports", "extra"},
		{"apply", "--turn", "t"},
		{"apply", "--event", "0123456789abcdef0123456789abcdef"},
		{"apply", "--event", "0123456789abcdef0123456789abcdef", "--turn", "t", "--wait", "-1s"},
	} {
		if code := w.run(args...); code != ExitUsage {
			t.Errorf("%v: exit %d\n%s", args, code, w.errOut.String())
		}
	}
}

// apply answers with its state and its exit: 0 applied, 1 refused, 3 the verdict is not in the journal yet.
func TestApplyCommandStatesAndExits(t *testing.T) {
	w := newCLIWorld(t)
	const event = "0123456789abcdef0123456789abcdef"
	info := apply.EventInfo{ID: event, ChildTask: "child-task", ParentTask: "orch-task", Head: strings.Repeat("a", 40), CriteriaDigest: "d"}
	var known = true
	saved := applyDeps
	t.Cleanup(func() { applyDeps = saved })
	applyDeps = func(ctx context.Context, env Environment, state, socket, repo string) (apply.Deps, func(), error) {
		st, _, err := env.Open(ctx, state, socket)
		if err != nil {
			return apply.Deps{}, nil, err
		}
		return apply.Deps{Store: st, Events: events(func() (apply.EventInfo, error) {
			if !known {
				return apply.EventInfo{}, apply.ErrUnknownEvent
			}
			return info, nil
		}), Commits: func(context.Context, string, string, string) ([]string, error) { return nil, nil },
			Receive: func(context.Context, apply.ReceiveInput) (json.RawMessage, error) { return json.RawMessage(`{}`), nil }}, func() { _ = st.Close() }, nil
	}
	// the child has no run yet: apply again in a later turn
	if code := w.run("apply", "--event", event, "--turn", "turn-1"); code != ExitNextTurn {
		t.Fatalf("exit %d\n%s", code, w.errOut.String())
	}
	var res apply.Result
	w.json(&res)
	if res.State != apply.StateNextTurn || res.Event != event {
		t.Errorf("result %+v", res)
	}
	known = false
	if code := w.run("apply", "--event", event, "--turn", "turn-1"); code != ExitRefused {
		t.Fatalf("exit %d\n%s", code, w.errOut.String())
	}
	w.json(&res)
	if res.State != apply.StateRefused || res.Reason != apply.ReasonUnknownEvent || !strings.Contains(w.errOut.String(), "unknown_event") {
		t.Errorf("result %+v stderr %s", res, w.errOut.String())
	}
}

type events func() (apply.EventInfo, error)

func (e events) Event(context.Context, string) (apply.EventInfo, error) { return e() }

// A report-up in a relay the operator selected (--state, --socket) sends its notice with that relay named, so the notice is not read and
// delivered against the manage configuration's relay; the hook of a resumed run does the same. (post-evaluation d6)
func TestReportUpAndTheResumeHookNameTheSelectedRelay(t *testing.T) {
	w := newCLIWorld(t)
	dir := t.TempDir()
	record := filepath.Join(dir, "argv.txt")
	program := filepath.Join(dir, "crw")
	script := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> " + record + "\nprintf '%s\\n' '{\"class\":\"accepted\",\"requestId\":\"r1\"}'\n"
	syscall.ForkLock.RLock()
	err := os.WriteFile(program, []byte(script), 0o700)
	syscall.ForkLock.RUnlock()
	if err != nil {
		t.Fatal(err)
	}
	w.env.Sender, w.env.Program = nil, program
	evidence := filepath.Join(t.TempDir(), "evidence.md")
	if err := os.WriteFile(evidence, []byte("blocked on a decision"), 0o600); err != nil {
		t.Fatal(err)
	}
	if code := w.run("report-up", "--kind", "project_blocked", "--run", "run-1", "--evidence", evidence, "--state", "/relay/selected", "--socket", "/sock/selected"); code != ExitOK {
		t.Fatalf("report-up: exit %d %s", code, w.errOut.String())
	}
	raw, err := os.ReadFile(record)
	if err != nil {
		t.Fatal(err)
	}
	line := string(raw)
	// the state is the one the command resolved (the test's Open answers the store's directory), the socket the one named
	if !strings.Contains(line, "--role supervisor") || !strings.Contains(line, "--state "+filepath.Dir(w.st.Path)) || !strings.Contains(line, "--socket /sock/selected") {
		t.Errorf("send-parent was called as %q", line)
	}
	hook, ok := UpwardFactory(w.st, nil, "run-1", "/relay/resumed", "/sock/resumed").(*report.RunHook)
	if !ok {
		t.Fatal("the factory makes no RunHook")
	}
	if sender, _ := hook.Sender.(report.ManageSender); sender.State != "/relay/resumed" || sender.Socket != "/sock/resumed" {
		t.Errorf("the hook's sender is %+v", hook.Sender)
	}
}

// audit (CRW-1206) goes through Run like the reach-up commands: the binary asks Handles once and Run serves audit with the Env it
// installed.
func TestRunServesTheAuditCommand(t *testing.T) {
	fixedClock(t)
	useEnv(t, acceptedWithNoReview(true), true, nil)
	if !Handles([]string{"audit"}) {
		t.Fatal("audit is not handled")
	}
	var out, errOut bytes.Buffer
	if code := Run(ctx, []string{"audit", "--run", "run-1", "--no-record"}, strings.NewReader(""), &out, &errOut); code != 1 || !strings.Contains(out.String(), "1 mismatch") {
		t.Errorf("exit %d stdout %q stderr %q", code, out.String(), errOut.String())
	}
}
