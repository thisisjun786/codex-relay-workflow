package role

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/host"
)

// dispatchHandoffHost is an App Server that answers the two reads the termination observation makes: thread/read with the
// child's identity and status (readErr: the host cannot answer), thread/turns/list with one newest turn ("" none).
type dispatchHandoffHost struct {
	status, parent, newest string
	readErr, listErr       error
	methods                []string
}

func (h *dispatchHandoffHost) Call(_ context.Context, method string, args map[string]any) (json.RawMessage, error) {
	h.methods = append(h.methods, method)
	switch method {
	case "thread/read":
		if h.readErr != nil {
			return nil, h.readErr
		}
		thread := map[string]any{"id": args["threadId"], "parentThreadId": createdRuntimeOr(h.parent, "session-test"), "threadSource": "subagent", "status": map[string]any{"type": createdRuntimeOr(h.status, "idle")}}
		return must(json.Marshal(map[string]any{"thread": thread})), nil
	case "thread/turns/list":
		if h.listErr != nil {
			return nil, h.listErr
		}
		data := []any{}
		if h.newest != "" {
			data = append(data, map[string]any{"id": "turn-2", "status": h.newest})
		}
		return must(json.Marshal(map[string]any{"data": data})), nil
	}
	return nil, errors.New("unexpected host call " + method)
}

// dispatchHandoffStart is a dispatch whose first attempt recorded child-a through the parity ledger. fallback adds the
// configured first fallback, so a handoff appends attempt 2; without it the handoff is main-direct.
func dispatchHandoffStart(t *testing.T, fallback bool) (ws string, env host.LookupEnv, attempt, file string) {
	t.Helper()
	ws = t.TempDir()
	env, dir := home(t)
	if fallback {
		writeStore(t, dir, `{"roles":{"executor":{"mode":"model","model":"xai/grok-4.6","effort":"high","fallback":{"model":"cursor/grok-4.6","effort":null}}}}`)
	}
	start := dispatchTestCall(t, ws, env, map[string]any{"action": "start", "role": "executor"})
	dispatchTestCall(t, ws, env, map[string]any{"action": "claim", "attemptId": start.AttemptID})
	dispatchTestCall(t, ws, env, map[string]any{"action": "report", "attemptId": start.AttemptID, "outcome": "created", "agentId": "child-a"})
	return ws, env, start.AttemptID, filepath.Join(ws, ".crw", "dispatches", "session-test", "task-test.json")
}

// dispatchHandoffReports are the two reports that hand a recorded, stopped child's attempt on.
func dispatchHandoffReports(attempt string) map[string]map[string]any {
	base := func(outcome string) map[string]any {
		return map[string]any{"action": "report", "sessionId": "session-test", "dispatchId": "task-test", "attemptId": attempt, "outcome": outcome, "agentId": "child-a", "executionState": "stopped", "reconciliation": "child stopped; partial work inspected"}
	}
	failed, task := base("failed"), base("task_failed")
	failed["error"] = "insufficient_quota"
	task["taskFailure"] = map[string]any{"kind": "unusable_output", "evidence": "final output unrelated to the packet"}
	return map[string]map[string]any{"failed": failed, "task_failed": task}
}

// A failed or task_failed report of a recorded child hands the attempt on only once the child is seen to have ended. A child
// that is active or whose newest turn runs refuses and writes nothing; a contradiction, a host that cannot answer and a child
// the host cannot see leave the attempt to reconcile with the child's id kept; a child of another parent refuses. None of them
// adds attempt 2 or reaches main-direct.
func TestDispatchHandoffRequiresTheRecordedChildToHaveEnded(t *testing.T) {
	for _, outcome := range []string{"failed", "task_failed"} {
		for _, tc := range []struct {
			name   string
			h      *dispatchHandoffHost
			refuse string // the refusal; "" leaves the attempt to reconcile
		}{
			{"status active", &dispatchHandoffHost{status: "active"}, "recorded child is active; stop it before handoff"},
			{"newest turn in progress", &dispatchHandoffHost{newest: "inProgress"}, "recorded child has a turn in progress; stop it before handoff"},
			{"active and in progress", &dispatchHandoffHost{status: "active", newest: "inProgress"}, "recorded child has a turn in progress; stop it before handoff"},
			{"status and turn disagree", &dispatchHandoffHost{status: "active", newest: "completed"}, ""},
			{"host unavailable", &dispatchHandoffHost{readErr: errors.New("host unavailable")}, ""},
			{"no turn seen", &dispatchHandoffHost{listErr: errors.New("turn list unavailable")}, ""},
			{"parent mismatch", &dispatchHandoffHost{parent: "other-session", newest: "completed"}, "recorded child is not a subagent of this session; the handoff cannot confirm it ended"},
		} {
			for _, fallback := range []bool{true, false} {
				t.Run(outcome+"/"+tc.name+map[bool]string{true: "/fallback", false: "/main-direct"}[fallback], func(t *testing.T) {
					ws, env, attempt, file := dispatchHandoffStart(t, fallback)
					before := must(os.ReadFile(file))
					h := *tc.h
					out, err := CheckedDispatch(context.Background(), ws, dispatchHandoffReports(attempt)[outcome], env, &h)
					stored := must(dispatchRead(file, "session-test", "task-test"))
					if tc.refuse != "" {
						if err == nil || err.Error() != tc.refuse {
							t.Fatalf("handoff refusal = %v, want %q", err, tc.refuse)
						}
						if string(before) != string(must(os.ReadFile(file))) {
							t.Fatal("the refused handoff wrote state")
						}
						return
					}
					check(t, err)
					if out.Action != "reconcile" || len(stored.Attempts) != 1 || !dispatchIs(stored.Status, "active") || !dispatchIs(stored.Attempts[0].Status, "reconcile") || *stored.Attempts[0].AgentID != "child-a" {
						t.Fatalf("unconfirmed handoff = %q %q, stored %v %v", out.Action, out.Reason, stored.Status, stored.Attempts[0].Status)
					}
				})
			}
		}
	}
}

// A newest turn that completed, was interrupted or failed hands the attempt on exactly once; the replayed report is stale.
func TestDispatchHandoffOnceAfterATerminalTurn(t *testing.T) {
	for _, outcome := range []string{"failed", "task_failed"} {
		for _, newest := range []string{"completed", "interrupted", "failed"} {
			for _, fallback := range []bool{true, false} {
				t.Run(outcome+"/"+newest+map[bool]string{true: "/fallback", false: "/main-direct"}[fallback], func(t *testing.T) {
					ws, env, attempt, file := dispatchHandoffStart(t, fallback)
					report := dispatchHandoffReports(attempt)[outcome]
					h := &dispatchHandoffHost{newest: newest}
					out, err := CheckedDispatch(context.Background(), ws, report, env, h)
					check(t, err)
					want := map[bool]string{true: "ready", false: "main-direct"}[fallback]
					if out.Action != want || len(h.methods) != 2 {
						t.Fatalf("handoff = %q after %v", out.Action, h.methods)
					}
					after := must(os.ReadFile(file))
					again, err := CheckedDispatch(context.Background(), ws, report, env, &dispatchHandoffHost{newest: newest})
					if fallback {
						if err == nil || !strings.Contains(err.Error(), "stale or missing attemptId") {
							t.Fatalf("replayed handoff = %v", err)
						}
					} else if check(t, err); again.Action != "main-direct" {
						t.Fatalf("replayed handoff = %q", again.Action)
					}
					if string(after) != string(must(os.ReadFile(file))) {
						t.Fatal("the replay changed the record")
					}
					stored := must(dispatchRead(file, "session-test", "task-test"))
					if len(stored.Attempts) != map[bool]int{true: 2, false: 1}[fallback] {
						t.Fatalf("attempts = %d", len(stored.Attempts))
					}
				})
			}
		}
	}
}

// A failure with no child (not_created and no recorded agent) needs no observation and asks the host nothing.
func TestDispatchHandoffWithoutAChildAsksNothing(t *testing.T) {
	ws := t.TempDir()
	env, _ := home(t)
	start := dispatchTestCall(t, ws, env, map[string]any{"action": "start", "role": "executor"})
	dispatchTestCall(t, ws, env, map[string]any{"action": "claim", "attemptId": start.AttemptID})
	h := &dispatchHandoffHost{readErr: errors.New("never asked")}
	out, err := CheckedDispatch(context.Background(), ws, map[string]any{"action": "report", "sessionId": "session-test", "dispatchId": "task-test", "attemptId": start.AttemptID, "outcome": "failed", "error": "insufficient_quota", "executionState": "not_created", "reconciliation": "no native child was created"}, env, h)
	check(t, err)
	if out.Action != "main-direct" || len(h.methods) != 0 {
		t.Fatalf("no-child handoff = %q after %v", out.Action, h.methods)
	}
}

// dispatchHandoffNative seeds a native thread database whose child-a row names a rollout file holding events, and returns
// the environment that reads it.
func dispatchHandoffNative(t *testing.T, env host.LookupEnv, events ...map[string]any) host.LookupEnv {
	t.Helper()
	native := t.TempDir()
	rollout := filepath.Join(native, "rollout-child-a.jsonl")
	var lines []string
	for _, e := range events {
		lines = append(lines, string(must(json.Marshal(map[string]any{"timestamp": "2026-10-10T00:00:00Z", "type": "event_msg", "payload": e}))))
	}
	check(t, os.WriteFile(rollout, []byte(strings.Join(lines, "\n")+"\n"), 0o600))
	db := must(sql.Open("sqlite", (&url.URL{Scheme: "file", Path: filepath.Join(native, "state_5.sqlite")}).String()))
	defer db.Close()
	_, err := db.Exec("CREATE TABLE threads (id TEXT PRIMARY KEY, rollout_path TEXT, source TEXT, archived INTEGER)")
	check(t, err)
	source := string(must(json.Marshal(map[string]any{"subagent": map[string]any{"thread_spawn": map[string]any{"parent_thread_id": "session-test", "depth": 1}}})))
	_, err = db.Exec("INSERT INTO threads VALUES (?,?,?,0)", "child-a", rollout, source)
	check(t, err)
	return func(k string) (string, bool) {
		if k == "CODEX_HOME" {
			return native, true
		}
		return env(k)
	}
}

// A native helper the App Server cannot see is judged by the host's own record of its turns: the child's rollout. A newest
// turn the rollout shows ended hands on; one it shows started and not ended stays to reconcile.
func TestDispatchHandoffNativeRolloutEvidence(t *testing.T) {
	started := map[string]any{"type": "task_started", "turn_id": "turn-1"}
	for _, tc := range []struct {
		name   string
		events []map[string]any
		action string
	}{
		{"complete", []map[string]any{started, {"type": "task_complete", "turn_id": "turn-1"}}, "ready"},
		{"aborted", []map[string]any{started, {"type": "turn_aborted", "turn_id": "turn-1"}}, "ready"},
		{"later turn running", []map[string]any{started, {"type": "task_complete", "turn_id": "turn-1"}, {"type": "task_started", "turn_id": "turn-2"}}, "reconcile"},
		{"no turn", nil, "reconcile"},
	} {
		for _, viaHost := range []bool{false, true} {
			t.Run(tc.name+map[bool]string{false: "/no host", true: "/host cannot see it"}[viaHost], func(t *testing.T) {
				ws, env, attempt, file := dispatchHandoffStart(t, true)
				env = dispatchHandoffNative(t, env, tc.events...)
				var h DispatchHost
				if viaHost {
					h = &dispatchHandoffHost{readErr: errors.New("thread not loaded")}
				}
				out, err := CheckedDispatch(context.Background(), ws, dispatchHandoffReports(attempt)["task_failed"], env, h)
				check(t, err)
				stored := must(dispatchRead(file, "session-test", "task-test"))
				if out.Action != tc.action || len(stored.Attempts) != map[string]int{"ready": 2, "reconcile": 1}[tc.action] {
					t.Fatalf("native handoff = %q %q with %d attempts", out.Action, out.Reason, len(stored.Attempts))
				}
			})
		}
	}
}

// dispatchHandoffEnded gives the child id of the native database in native a rollout whose one turn completed, adding the
// rollout_path column to a seeded table that lacks it.
func dispatchHandoffEnded(t *testing.T, native, id string) {
	t.Helper()
	rollout := filepath.Join(native, "rollout-"+id+".jsonl")
	var lines []string
	for _, e := range []map[string]any{{"type": "task_started", "turn_id": "turn-1"}, {"type": "task_complete", "turn_id": "turn-1"}} {
		lines = append(lines, string(must(json.Marshal(map[string]any{"type": "event_msg", "payload": e}))))
	}
	check(t, os.WriteFile(rollout, []byte(strings.Join(lines, "\n")+"\n"), 0o600))
	db := must(sql.Open("sqlite", (&url.URL{Scheme: "file", Path: filepath.Join(native, "state_5.sqlite")}).String()))
	defer db.Close()
	if _, err := db.Exec("ALTER TABLE threads ADD COLUMN rollout_path TEXT"); err != nil && !strings.Contains(err.Error(), "duplicate column") {
		t.Fatal(err)
	}
	_, err := db.Exec("UPDATE threads SET rollout_path=? WHERE id=?", rollout, id)
	check(t, err)
}

// The CLI asks its opener for a host for a failed report too, and the handoff it permits is the one the host's observation
// allows: the active child refuses, and once its newest turn completed the attempt is handed on.
func TestDispatchCommandHandoffReadsTheHost(t *testing.T) {
	dispatchHostRestoreOpen(t)
	ws := t.TempDir()
	t.Chdir(ws)
	env := dispatchHostEnv(t, dispatchHostNative(t))
	attempt, file := dispatchHostStarted(t, ws, env)
	calls := 0
	h := &dispatchHandoffHost{status: "active"}
	OpenDispatchHost = func(host.LookupEnv) (DispatchHost, func(), error) {
		calls++
		return h, func() {}, nil
	}
	report := dispatchHandoffReports(attempt)["failed"]
	delete(report, "sessionId")
	delete(report, "dispatchId")
	before := must(os.ReadFile(file))
	if _, err := dispatchHostCommand(t, env, report); err == nil || err.Error() != "recorded child is active; stop it before handoff" {
		t.Fatalf("CLI handoff of an active child = %v", err)
	}
	if string(before) != string(must(os.ReadFile(file))) {
		t.Fatal("the refused handoff wrote state")
	}
	h.status, h.newest = "idle", "completed"
	out, err := dispatchHostCommand(t, env, report)
	check(t, err)
	if out.Action != "main-direct" || calls != 2 {
		t.Fatalf("CLI handoff = %q after %d opens", out.Action, calls)
	}
}

// dispatchHandoffTail appends raw bytes to the rollout dispatchHandoffNative wrote for child-a.
func dispatchHandoffTail(t *testing.T, env host.LookupEnv, tail string) {
	t.Helper()
	native, _ := env("CODEX_HOME")
	f := must(os.OpenFile(filepath.Join(native, "rollout-child-a.jsonl"), os.O_APPEND|os.O_WRONLY, 0o600))
	defer f.Close()
	must(f.WriteString(tail))
}

// A rollout whose last lines are unfinished or damaged does not show how the newest turn stands: an earlier turn's end must
// not stand for it. A partial task_started after a whole turn, and a damaged event line, leave the handoff to reconcile and
// the cleanup unconfirmed; a later whole turn is read again.
func TestDispatchHandoffDamagedRolloutDoesNotShowAnEnd(t *testing.T) {
	whole := []map[string]any{{"type": "task_started", "turn_id": "turn-1"}, {"type": "task_complete", "turn_id": "turn-1"}}
	for _, tc := range []struct{ name, tail, action string }{
		{"partial task_started", `{"type":"event_msg","payload":{"type":"task_started","turn_id":"turn-2"`, "reconcile"},
		{"damaged line", "{\"type\":\"event_msg\",\"payload\":{\"type\":\"task_started\",\"turn_id\":\"turn-2\"\n", "reconcile"},
		{"partial prefix", `{"timestamp":"2026-10-10T00:00:00Z","ordin`, "reconcile"},
		{"later whole turn", "{\"type\":\"event_msg\",\"payload\":{\"type\":\"task_started\",\"turn_id\":\"turn-2\"\n" +
			`{"type":"event_msg","payload":{"type":"task_started","turn_id":"turn-3"}}` + "\n" + `{"type":"event_msg","payload":{"type":"task_complete","turn_id":"turn-3"}}` + "\n", "ready"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ws, env, attempt, file := dispatchHandoffStart(t, true)
			env = dispatchHandoffNative(t, env, whole...)
			dispatchHandoffTail(t, env, tc.tail)
			out, err := CheckedDispatch(context.Background(), ws, dispatchHandoffReports(attempt)["task_failed"], env, nil)
			check(t, err)
			stored := must(dispatchRead(file, "session-test", "task-test"))
			if out.Action != tc.action || len(stored.Attempts) != map[string]int{"ready": 2, "reconcile": 1}[tc.action] {
				t.Fatalf("handoff after %s = %q %q with %d attempts", tc.name, out.Action, out.Reason, len(stored.Attempts))
			}
		})
	}
}

// The cleanup of a policy-stopped dispatch reads the same rollout: a damaged tail leaves the child's cleanup unconfirmed and
// its id held.
func TestDispatchCleanupDamagedRolloutStaysUnconfirmed(t *testing.T) {
	ws, env, attempt, file := dispatchHandoffStart(t, true)
	stop := map[string]any{"action": "report", "sessionId": "session-test", "dispatchId": "task-test", "attemptId": attempt, "outcome": "failed", "error": "permission_denied", "executionState": "running"}
	_, err := CheckedDispatch(context.Background(), ws, stop, env, nil)
	check(t, err)
	env = dispatchHandoffNative(t, env, map[string]any{"type": "task_started", "turn_id": "turn-1"}, map[string]any{"type": "task_complete", "turn_id": "turn-1"})
	dispatchHandoffTail(t, env, `{"type":"event_msg","payload":{"type":"task_started","turn_id":"turn-2"`)
	out, err := CheckedDispatch(context.Background(), ws, dispatchCleanupStop(attempt), env, nil)
	check(t, err)
	a := must(dispatchRead(file, "session-test", "task-test")).Attempts[0]
	if out.Action != "stop" || a.Cleanup == nil || a.Cleanup.Status != "unconfirmed" {
		t.Fatalf("cleanup after a damaged tail = %q %+v", out.Action, a.Cleanup)
	}
}
