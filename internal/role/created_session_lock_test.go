package role

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

// createdLockHost answers every thread read as a child of session-test whose id is the one asked for. With hold set it
// blocks its first read until the test lets it go: a created report reads the host after it has scanned the records of
// its session and before it writes its own, so that is the moment the report is held.
type createdLockHost struct {
	mu     sync.Mutex
	reads  int
	status string
	err    error
	onRead func()        // runs at the start of every read, still inside the locks of the call
	held   chan struct{} // closed once the first read is blocked
	hold   chan struct{} // closed by the test to let it go
}

func (h *createdLockHost) Call(ctx context.Context, _ string, args map[string]any) (json.RawMessage, error) {
	if h.onRead != nil {
		h.onRead()
	}
	h.mu.Lock()
	h.reads++
	block := h.reads == 1 && h.hold != nil
	h.mu.Unlock()
	if block {
		close(h.held)
		select {
		case <-h.hold:
		case <-ctx.Done():
		case <-time.After(10 * time.Second):
		}
	}
	if h.err != nil {
		return nil, h.err
	}
	return must(json.Marshal(map[string]any{"thread": map[string]any{"id": args["threadId"], "parentThreadId": "session-test", "threadSource": "subagent", "status": map[string]any{"type": h.status}}})), nil
}

func createdLockStop(dispatch, attempt, agent string) map[string]any {
	return map[string]any{"action": "report", "sessionId": "session-test", "dispatchId": dispatch, "attemptId": attempt, "outcome": "stopped", "executionState": "stopped", "agentId": agent, "reconciliation": "reported by mistake; the child belongs to another dispatch"}
}

// createdLockHolders lists the dispatches whose records hold the agent, whatever the state of the attempt.
func createdLockHolders(ws, agent string) []string {
	var out []string
	for _, path := range must(filepath.Glob(createdArchivedRecord(ws, "*"))) {
		id := strings.TrimSuffix(filepath.Base(path), ".json")
		for _, a := range must(dispatchRead(path, "session-test", id)).Attempts {
			if a.AgentID != nil && *a.AgentID == agent {
				out = append(out, id)
			}
		}
	}
	return out
}

// Two dispatches of one session that report the same child at the same time: the report that is not held by the host read
// must not get past the other, and once the first has written, the second is refused as a replay.
func TestCreatedSessionLockAdmitsOneReportOfAnAgent(t *testing.T) {
	ws := t.TempDir()
	env, _ := home(t)
	h := &createdLockHost{status: "idle", held: make(chan struct{}), hold: make(chan struct{})}
	release := sync.OnceFunc(func() { close(h.hold) })
	defer release()
	one, two := createdArchivedSession(t, ws, env, "task-one"), createdArchivedSession(t, ws, env, "task-two")
	first := make(chan error, 1)
	go func() {
		_, err := CheckedDispatch(context.Background(), ws, createdArchivedReport("task-one", one, "child-a"), env, h)
		first <- err
	}()
	select {
	case <-h.held:
	case <-time.After(10 * time.Second):
		t.Fatal("the first report never reached the host")
	}
	_, err := CheckedDispatch(context.Background(), ws, createdArchivedReport("task-two", two, "child-a"), env, h)
	if err == nil || !strings.Contains(err.Error(), ".session.lock: file exists") {
		t.Errorf("report of the same child while the first is held: %v", err)
	}
	release()
	select {
	case err := <-first:
		check(t, err)
	case <-time.After(10 * time.Second):
		t.Fatal("the first report did not finish")
	}
	_, err = CheckedDispatch(context.Background(), ws, createdArchivedReport("task-two", two, "child-a"), env, h)
	if err == nil || !strings.Contains(err.Error(), "agentId was already reported for dispatch task-one attempt "+one) {
		t.Errorf("report of the same child after the first finished: %v", err)
	}
	if got := createdLockHolders(ws, "child-a"); !reflect.DeepEqual(got, []string{"task-one"}) {
		t.Errorf("dispatches holding child-a = %v, want only task-one", got)
	}
}

// The refusal text sends the caller to the stopped close, so that close has to free the id for the dispatch that really
// spawned the child, and the child stays recorded as evidence in the closed attempt.
func TestCreatedSessionLockStoppedCloseFreesTheAgent(t *testing.T) {
	ws := t.TempDir()
	env, _ := home(t)
	h := &createdLockHost{status: "idle"}
	one, two := createdArchivedSession(t, ws, env, "task-one"), createdArchivedSession(t, ws, env, "task-two")
	report := func(dispatch, attempt string) (DispatchResult, error) {
		return CheckedDispatch(context.Background(), ws, createdArchivedReport(dispatch, attempt, "child-a"), env, h)
	}
	_, err := report("task-one", one)
	check(t, err)
	if _, err = report("task-two", two); err == nil || !strings.Contains(err.Error(), "already reported for dispatch task-one") {
		t.Fatalf("report of a held child: %v", err)
	}
	out, err := CheckedDispatch(context.Background(), ws, createdLockStop("task-one", one, "child-a"), env, h)
	check(t, err)
	if out.Action != "stop" {
		t.Fatalf("close = %+v", out)
	}
	if out, err = report("task-two", two); err != nil || out.Action != "wait" {
		t.Fatalf("report of the child after the wrong report was closed: %q, %v", out.Action, err)
	}
	done := createdArchivedReport("task-two", two, "child-a")
	done["outcome"] = "complete"
	_, err = CheckedDispatch(context.Background(), ws, done, env, h)
	check(t, err)
	if stored := must(dispatchRead(createdArchivedRecord(ws, "task-two"), "session-test", "task-two")); stored.Status != "complete" {
		t.Fatalf("task-two = %v", stored.Status)
	}
	closed := must(dispatchRead(createdArchivedRecord(ws, "task-one"), "session-test", "task-one"))
	a := closed.Attempts[0]
	if closed.Status != "stopped" || a.Status != "failed" || a.Reconciliation == nil || a.AgentID == nil || *a.AgentID != "child-a" {
		t.Fatalf("the closed record lost its evidence: %+v", closed)
	}
	three := createdArchivedSession(t, ws, env, "task-three")
	if _, err = report("task-three", three); err == nil || !strings.Contains(err.Error(), "already reported for dispatch task-two") {
		t.Fatalf("report of a child task-two holds: %v", err)
	}
}

// An id stays held by an attempt that is still running and by a finished dispatch, and the refusal says which way out
// exists: only a dispatch the stopped close can still close frees its id, so a finished one is not told to close.
func TestCreatedSessionLockRefusalSaysHowAnAgentIsFreed(t *testing.T) {
	for _, tc := range []struct {
		name     string
		complete bool
		has      string
		lacks    string
	}{
		{"running attempt", false, "once dispatch task-one is closed that way its agentId is free", "spawn a new child"},
		{"completed dispatch", true, "spawn a new child for this attempt", "close an old wrong-ID report"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ws := t.TempDir()
			env, _ := home(t)
			h := &createdLockHost{status: "idle"}
			one, two := createdArchivedSession(t, ws, env, "task-one"), createdArchivedSession(t, ws, env, "task-two")
			_, err := CheckedDispatch(context.Background(), ws, createdArchivedReport("task-one", one, "child-a"), env, h)
			check(t, err)
			if tc.complete {
				done := createdArchivedReport("task-one", one, "child-a")
				done["outcome"] = "complete"
				_, err = CheckedDispatch(context.Background(), ws, done, env, h)
				check(t, err)
			}
			_, err = CheckedDispatch(context.Background(), ws, createdArchivedReport("task-two", two, "child-a"), env, h)
			if err == nil || !strings.Contains(err.Error(), "agentId was already reported for dispatch task-one attempt "+one) || !strings.Contains(err.Error(), "copy agentId") ||
				!strings.Contains(err.Error(), tc.has) || strings.Contains(err.Error(), tc.lacks) {
				t.Fatalf("refusal = %v", err)
			}
			if got := createdLockHolders(ws, "child-a"); !reflect.DeepEqual(got, []string{"task-one"}) {
				t.Fatalf("dispatches holding child-a = %v", got)
			}
		})
	}
}

// Only the attempt the stopped close closed is released: an earlier attempt of the same dispatch that was handed off keeps
// its child, though its dispatch is stopped now.
func TestCreatedSessionLockEarlierAttemptKeepsItsAgent(t *testing.T) {
	ws := t.TempDir()
	env, dir := home(t)
	writeStore(t, dir, `{"roles":{"executor":{"mode":"model","model":"a/one","fallback":{"model":"b/two","effort":null}}}}`)
	h := &createdLockHost{status: "idle"}
	first := createdArchivedSession(t, ws, env, "task-one")
	_, err := CheckedDispatch(context.Background(), ws, createdArchivedReport("task-one", first, "child-a"), env, h)
	check(t, err)
	next := dispatchTestCall(t, ws, env, map[string]any{"action": "report", "dispatchId": "task-one", "attemptId": first, "outcome": "failed", "error": "insufficient_quota", "executionState": "stopped", "agentId": "child-a", "reconciliation": "first child stopped; partial work inspected"})
	dispatchTestClaimIssued(t, ws, env, map[string]any{"action": "claim", "dispatchId": "task-one", "attemptId": next.AttemptID})
	_, err = CheckedDispatch(context.Background(), ws, createdArchivedReport("task-one", next.AttemptID, "child-b"), env, h)
	check(t, err)
	_, err = CheckedDispatch(context.Background(), ws, createdLockStop("task-one", next.AttemptID, "child-b"), env, h)
	check(t, err)
	two := createdArchivedSession(t, ws, env, "task-two")
	if _, err = CheckedDispatch(context.Background(), ws, createdArchivedReport("task-two", two, "child-a"), env, h); err == nil || !strings.Contains(err.Error(), "already reported for dispatch task-one attempt "+first) {
		t.Fatalf("report of the child of an earlier attempt: %v", err)
	}
	out, err := CheckedDispatch(context.Background(), ws, createdArchivedReport("task-two", two, "child-b"), env, h)
	if err != nil || out.Action != "wait" {
		t.Fatalf("report of the child the stopped close released: %q, %v", out.Action, err)
	}
}

// A held session lock is the busy answer of a created report and of the stopped close, and nothing is stolen; the other
// actions never take it; every path that took it gives it back.
func TestCreatedSessionLockBusyAndReleased(t *testing.T) {
	ws := t.TempDir()
	env, _ := home(t)
	h := &createdLockHost{status: "idle"}
	one, two := createdArchivedSession(t, ws, env, "task-one"), createdArchivedSession(t, ws, env, "task-two")
	lock := filepath.Join(filepath.Dir(createdArchivedRecord(ws, "x")), ".session.lock")
	gone := func(when string) {
		t.Helper()
		if _, err := os.Lstat(lock); !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("%s left the session lock behind: %v", when, err)
		}
	}
	report := func(dispatch, attempt, agent string, host DispatchHost) error {
		_, err := CheckedDispatch(context.Background(), ws, createdArchivedReport(dispatch, attempt, agent), env, host)
		return err
	}
	check(t, report("task-one", one, "child-a", h))
	check(t, report("task-two", two, "child-b", h))
	gone("a report")
	before := [2]string{string(must(os.ReadFile(createdArchivedRecord(ws, "task-one")))), string(must(os.ReadFile(createdArchivedRecord(ws, "task-two"))))}
	h.reads = 0
	check(t, os.Mkdir(lock, 0o700))
	if err := report("task-one", one, "child-a", h); err == nil || !strings.Contains(err.Error(), ".session.lock: file exists") {
		t.Errorf("created report with the session lock held: %v", err)
	}
	_, err := CheckedDispatch(context.Background(), ws, createdLockStop("task-two", two, "child-b"), env, h)
	if err == nil || !strings.Contains(err.Error(), ".session.lock: file exists") {
		t.Errorf("stopped close with the session lock held: %v", err)
	}
	if after := [2]string{string(must(os.ReadFile(createdArchivedRecord(ws, "task-one")))), string(must(os.ReadFile(createdArchivedRecord(ws, "task-two"))))}; after != before || h.reads != 0 {
		t.Errorf("a busy answer changed a record or asked the host (%d reads)", h.reads)
	}
	three := createdArchivedSession(t, ws, env, "task-three")
	if _, err := os.Lstat(lock); err != nil {
		t.Fatalf("the planted session lock was taken away: %v", err)
	}
	// With both locks held the session lock is the one that answers: it is taken first.
	check(t, os.Mkdir(createdArchivedRecord(ws, "task-one")+".lock", 0o700))
	if err := report("task-one", one, "child-a", h); err == nil || !strings.Contains(err.Error(), ".session.lock: file exists") {
		t.Errorf("created report with both locks held: %v", err)
	}
	check(t, os.Remove(createdArchivedRecord(ws, "task-one")+".lock"))
	check(t, os.Remove(lock))
	// A dispatch lock that is held is the busy answer as before, and the session lock taken first is given back.
	check(t, os.Mkdir(createdArchivedRecord(ws, "task-one")+".lock", 0o700))
	if err := report("task-one", one, "child-a", h); err == nil || !strings.Contains(err.Error(), "task-one.json.lock: file exists") {
		t.Errorf("created report with its dispatch lock held: %v", err)
	}
	gone("a report that found its dispatch lock held")
	check(t, os.Remove(createdArchivedRecord(ws, "task-one")+".lock"))
	// The same for a stopped close whose dispatch lock is taken after its first status read.
	plant := func(point string) {
		if point == "session" {
			check(t, os.Mkdir(createdArchivedRecord(ws, "task-two")+".lock", 0o700))
		}
	}
	_, err = dispatchPinnedChecked(context.Background(), ws, createdLockStop("task-two", two, "child-b"), env, h, plant)
	if err == nil || !strings.Contains(err.Error(), "task-two.json.lock: file exists") {
		t.Errorf("stopped close with its dispatch lock held: %v", err)
	}
	gone("a stopped close that found its dispatch lock held")
	check(t, os.Remove(createdArchivedRecord(ws, "task-two")+".lock"))
	// Refusals of every kind give the lock back too.
	if err := report("task-three", three, "child-a", h); err == nil {
		t.Error("report of a held child accepted")
	}
	gone("a refused report")
	if err := report("task-three", three, "child-c", &createdLockHost{err: errors.New("host unavailable")}); err == nil {
		t.Error("report the host could not verify accepted")
	}
	gone("a report the host refused")
	_, err = CheckedDispatch(context.Background(), ws, createdLockStop("task-one", one, "child-a"), env, &createdLockHost{status: "active"})
	if err == nil || !strings.Contains(err.Error(), "active") {
		t.Errorf("stopped close of an active child: %v", err)
	}
	gone("a refused stopped close")
}

// Only the stopped close releases an id: a dispatch the provider failure stopped keeps its attempt running, and one handed
// to the main agent keeps a failed attempt with a reconciliation, and both go on holding their child.
func TestCreatedSessionLockOtherEndsKeepTheirAgent(t *testing.T) {
	for _, tc := range []struct{ name, store, code, want string }{
		{"provider failure", "", "invalid_api_key", "stopped"},
		{"handoff to the main agent", `{"roles":{"executor":{"mode":"model","model":"a/one"}}}`, "insufficient_quota", "main-direct"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ws := t.TempDir()
			env, dir := home(t)
			if tc.store != "" {
				writeStore(t, dir, tc.store)
			}
			h := &createdLockHost{status: "idle"}
			one, two := createdArchivedSession(t, ws, env, "task-one"), createdArchivedSession(t, ws, env, "task-two")
			_, err := CheckedDispatch(context.Background(), ws, createdArchivedReport("task-one", one, "child-a"), env, h)
			check(t, err)
			dispatchTestCall(t, ws, env, map[string]any{"action": "report", "dispatchId": "task-one", "attemptId": one, "outcome": "failed", "error": tc.code, "executionState": "stopped", "agentId": "child-a", "reconciliation": "child stopped"})
			if status := must(dispatchRead(createdArchivedRecord(ws, "task-one"), "session-test", "task-one")).Status; status != tc.want {
				t.Fatalf("task-one is %v", status)
			}
			if _, err = CheckedDispatch(context.Background(), ws, createdArchivedReport("task-two", two, "child-a"), env, h); err == nil || !strings.Contains(err.Error(), "already reported for dispatch task-one attempt "+one) {
				t.Fatalf("report of the child of a dispatch that was not closed: %v", err)
			}
		})
	}
}

// A lock that cannot be given back is an error of the call, not a silent leftover that refuses later reports.
func TestCreatedSessionLockReleaseFailureIsReported(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("a read-only directory does not stop root")
	}
	for _, tc := range []struct {
		name, lock string
		stop       bool
	}{
		{"created report, session lock", ".session.lock", false},
		{"stopped close, session lock", ".session.lock", true},
		{"created report, record lock", "task-one.json.lock", false},
		{"stopped close, record lock", "task-one.json.lock", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ws := t.TempDir()
			env, _ := home(t)
			stuck := filepath.Join(filepath.Dir(createdArchivedRecord(ws, "x")), tc.lock, "stuck")
			t.Cleanup(func() { _ = os.Chmod(stuck, 0o700) })
			// While the call holds the lock, a file in a read-only directory inside it makes the removal of the lock fail.
			// The close reads the host once for the identity and once for the newest turn, so the plant is made at the first
			// read and has to survive the second.
			plant := sync.OnceFunc(func() {
				check(t, os.Mkdir(stuck, 0o700))
				check(t, os.WriteFile(filepath.Join(stuck, "f"), nil, 0o600))
				check(t, os.Chmod(stuck, 0o500))
			})
			h := &createdLockHost{status: "idle", onRead: plant}
			one := createdArchivedSession(t, ws, env, "task-one")
			input := createdArchivedReport("task-one", one, "child-a")
			if tc.stop {
				_, err := CheckedDispatch(context.Background(), ws, input, env, &createdLockHost{status: "idle"})
				check(t, err)
				input = createdLockStop("task-one", one, "child-a")
			}
			if _, err := CheckedDispatch(context.Background(), ws, input, env, h); err == nil || !strings.Contains(err.Error(), "permission denied") {
				t.Fatalf("call that could not remove its session lock: %v", err)
			}
		})
	}
}
