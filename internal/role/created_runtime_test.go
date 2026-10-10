package role

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/host"
)

// createdRuntimeHost answers thread/read as the created check asks it (identity only) and thread/turns/list with the
// turns it was given, newest first. It never answers anything else.
type createdRuntimeHost struct {
	status, parent, source string          // the thread; idle, session-test and subagent when empty
	turnsField             string          // the turns list of the thread/read reply: "" is [], "absent" drops it, "null" is null
	turns                  json.RawMessage // the thread/turns/list answer
	turnErr                error
	block                  bool // thread/turns/list waits until its context ends
	methods                []string
	listArgs               map[string]any
}

func createdRuntimeOr(v, fallback string) string {
	if v == "" {
		return fallback
	}
	return v
}

func createdRuntimeNewest(status string) *createdRuntimeHost {
	return &createdRuntimeHost{turns: json.RawMessage(`{"data":[{"id":"turn-2","status":"` + status + `"},{"id":"turn-1","status":"completed"}]}`)}
}

func (h *createdRuntimeHost) Call(ctx context.Context, method string, args map[string]any) (json.RawMessage, error) {
	h.methods = append(h.methods, method)
	switch method {
	case "thread/read":
		if args["includeTurns"] != false {
			return nil, errors.New("thread/read must not ask for turns")
		}
		thread := map[string]any{"id": args["threadId"], "parentThreadId": createdRuntimeOr(h.parent, "session-test"), "threadSource": createdRuntimeOr(h.source, "subagent"), "status": map[string]any{"type": createdRuntimeOr(h.status, "idle")}}
		switch h.turnsField {
		case "":
			thread["turns"] = []any{}
		case "null":
			thread["turns"] = nil
		}
		return must(json.Marshal(map[string]any{"thread": thread})), nil
	case "thread/turns/list":
		h.listArgs = args
		if h.block {
			<-ctx.Done()
			return nil, ctx.Err()
		}
		return h.turns, h.turnErr
	}
	return nil, errors.New("unexpected host call " + method)
}

// createdRuntimeStart is a dispatch whose attempt reported child-a as created; the host (nil: the native database) vouched for it.
func createdRuntimeStart(t *testing.T, native bool) (ws string, env host.LookupEnv, attempt, file string) {
	t.Helper()
	ws, env, start, file := dispatchTestFixture(t)
	dispatchTestClaimIssued(t, ws, env, map[string]any{"action": "claim", "attemptId": start.AttemptID})
	var h DispatchHost = &createdRuntimeHost{}
	if native {
		dir, old := t.TempDir(), env
		createdCheckSeed(t, dir, "child-a", "session-test")
		env, h = func(k string) (string, bool) {
			if k == "CODEX_HOME" {
				return dir, true
			}
			return old(k)
		}, nil
	}
	_, err := CheckedDispatch(context.Background(), ws, createdCheckInput(start.AttemptID, "created"), env, h)
	check(t, err)
	return ws, env, start.AttemptID, file
}

func createdRuntimeStop(attempt string) map[string]any {
	input := createdCheckInput(attempt, "stopped")
	input["executionState"], input["reconciliation"] = "stopped", "inspected child"
	return input
}

const (
	createdRuntimeClosed = "dispatch closed; caller reconciliation recorded, recorded identity retained"
	createdRuntimeNote   = "runtime not confirmed"
)

// A child whose newest turn is still in progress is not closed, whatever status the thread reports and whatever the
// thread/read answer carries, and the refusal writes and keeps nothing.
func TestCreatedRuntimeRefusesANewestTurnInProgress(t *testing.T) {
	refuse := func(t *testing.T, h *createdRuntimeHost) {
		t.Helper()
		ws, env, attempt, file := createdRuntimeStart(t, false)
		before := must(os.ReadFile(file))
		_, err := CheckedDispatch(context.Background(), ws, createdRuntimeStop(attempt), env, h)
		if err == nil || err.Error() != "recorded child has a turn in progress; stop it before closing" {
			t.Fatalf("close of a child with a turn in progress: %v", err)
		}
		if h.listArgs["threadId"] != "child-a" || h.listArgs["limit"] != 1 || string(before) != string(must(os.ReadFile(file))) {
			t.Fatalf("turn list %v, or the record changed", h.listArgs)
		}
		if left := must(filepath.Glob(filepath.Join(filepath.Dir(file), ".session.lock*"))); len(left) != 0 || must(filepath.Glob(file+".*")) != nil {
			t.Fatalf("a lock or temporary file stayed: %v", left)
		}
	}
	for _, status := range []string{"idle", "notLoaded"} {
		t.Run(status, func(t *testing.T) {
			h := createdRuntimeNewest("inProgress")
			h.status = status
			refuse(t, h)
		})
	}
	// The turns field of the thread/read answer decides nothing: a host that omits it, or sends null, still gets the read.
	for _, tc := range []struct {
		name string
		h    *createdRuntimeHost
	}{
		{"no turns field", &createdRuntimeHost{turnsField: "absent", turns: createdRuntimeNewest("inProgress").turns}},
		{"turns null", &createdRuntimeHost{turnsField: "null", turns: createdRuntimeNewest("inProgress").turns}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			refuse(t, tc.h)
		})
	}
}

// Every other answer closes as before; the note says whether the newest turn was seen to end, and a thread that is not the recorded child is not asked.
func TestCreatedRuntimeClosesAndSaysWhetherTheRuntimeWasConfirmed(t *testing.T) {
	for _, tc := range []struct {
		name    string
		h       *createdRuntimeHost
		methods int // host calls of the close
		note    bool
	}{
		{"completed", createdRuntimeNewest("completed"), 2, false},
		{"interrupted", createdRuntimeNewest("interrupted"), 2, false},
		{"failed", createdRuntimeNewest("failed"), 2, false},
		{"unknown status", createdRuntimeNewest("paused"), 2, true},
		{"no turns", &createdRuntimeHost{turns: json.RawMessage(`{"data":[]}`)}, 2, true},
		{"undecodable list", &createdRuntimeHost{turns: json.RawMessage("{")}, 2, true},
		{"list error", &createdRuntimeHost{turnErr: errors.New("host unavailable")}, 2, true},
		{"foreign thread", &createdRuntimeHost{parent: "other", turns: createdRuntimeNewest("inProgress").turns}, 1, true},
		{"not a subagent", &createdRuntimeHost{source: "cli", turns: createdRuntimeNewest("inProgress").turns}, 1, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ws, env, attempt, file := createdRuntimeStart(t, false)
			out, err := CheckedDispatch(context.Background(), ws, createdRuntimeStop(attempt), env, tc.h)
			check(t, err)
			if out.Action != "stop" || !strings.HasPrefix(out.Reason, createdRuntimeClosed) || strings.Contains(out.Reason, createdRuntimeNote) != tc.note || len(tc.h.methods) != tc.methods {
				t.Fatalf("close = %q %q after %v", out.Action, out.Reason, tc.h.methods)
			}
			if stored := must(dispatchRead(file, "session-test", "task-test")); stored.Status != "stopped" || stored.Attempts[0].Reconciliation == nil {
				t.Fatalf("closure not durable: %+v", stored)
			}
		})
	}
}

// The active refusal keeps its text and its single host call, and the turn list is not asked after it.
func TestCreatedRuntimeActiveChildKeepsItsRefusal(t *testing.T) {
	ws, env, attempt, file := createdRuntimeStart(t, false)
	before := must(os.ReadFile(file))
	h := createdRuntimeNewest("completed")
	h.status = "active"
	if _, err := CheckedDispatch(context.Background(), ws, createdRuntimeStop(attempt), env, h); err == nil || err.Error() != "recorded child is active; stop it before closing" {
		t.Fatalf("close of an active child: %v", err)
	}
	if len(h.methods) != 1 || string(before) != string(must(os.ReadFile(file))) {
		t.Fatalf("calls %v, or the record changed", h.methods)
	}
}

// One bound covers the identity read and the turn read: a host that never answers the turn list does not hold the close.
func TestCreatedRuntimeTurnListIsBoundedByTheContext(t *testing.T) {
	ws, env, attempt, _ := createdRuntimeStart(t, false)
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	begin := time.Now()
	out, err := CheckedDispatch(ctx, ws, createdRuntimeStop(attempt), env, &createdRuntimeHost{block: true})
	if check(t, err); !strings.Contains(out.Reason, createdRuntimeNote) || time.Since(begin) > 10*time.Second {
		t.Fatalf("close = %q after %v", out.Reason, time.Since(begin))
	}
}

// The native database holds no turns: the close goes through with the note.
func TestCreatedRuntimeNativeDatabaseHasNoTurns(t *testing.T) {
	ws, env, attempt, _ := createdRuntimeStart(t, true)
	out, err := CheckedDispatch(context.Background(), ws, createdRuntimeStop(attempt), env, nil)
	check(t, err)
	if out.Action != "stop" || out.Reason != createdRuntimeClosed+"; "+createdRuntimeNote {
		t.Fatalf("close = %q %q", out.Action, out.Reason)
	}
}

// A refused close keeps the child's id held; the close that goes through frees it, and closing again asks the host nothing.
func TestCreatedRuntimeRefusedCloseKeepsTheIdAndAClosedOneIsFinal(t *testing.T) {
	ws := t.TempDir()
	env, _ := home(t)
	one, two := createdArchivedSession(t, ws, env, "task-one"), createdArchivedSession(t, ws, env, "task-two")
	_, err := CheckedDispatch(context.Background(), ws, createdArchivedReport("task-one", one, "child-a"), env, &createdRuntimeHost{})
	check(t, err)
	report := func() error {
		_, err := CheckedDispatch(context.Background(), ws, createdArchivedReport("task-two", two, "child-a"), env, &createdRuntimeHost{})
		return err
	}
	if _, err = CheckedDispatch(context.Background(), ws, createdLockStop("task-one", one, "child-a"), env, createdRuntimeNewest("inProgress")); err == nil || !strings.Contains(err.Error(), "turn in progress") {
		t.Fatalf("close with a turn in progress: %v", err)
	}
	if err = report(); err == nil || !strings.Contains(err.Error(), "already reported for dispatch task-one") {
		t.Fatalf("report of the child a refused close still holds: %v", err)
	}
	_, err = CheckedDispatch(context.Background(), ws, createdLockStop("task-one", one, "child-a"), env, createdRuntimeNewest("completed"))
	check(t, err)
	check(t, report())
	again := &createdRuntimeHost{}
	out, err := CheckedDispatch(context.Background(), ws, createdLockStop("task-one", one, "child-a"), env, again)
	if check(t, err); out.Action != "stop" || out.Reason != "" || len(again.methods) != 0 {
		t.Fatalf("second close = %q %q after %v", out.Action, out.Reason, again.methods)
	}
}
