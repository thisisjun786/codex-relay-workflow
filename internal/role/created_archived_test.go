package role

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/host"
)

// createdArchivedHost points the host thread database at a temporary CODEX_HOME that holds one
// child-a row with the given spawn parent, optional source override and archive flag. It returns
// the environment and the database path, so a test can prove the read left the database alone.
func createdArchivedHost(t *testing.T, env host.LookupEnv, parent, source string, archived int) (host.LookupEnv, string) {
	t.Helper()
	native := t.TempDir()
	createdCheckSeed(t, native, "child-a", parent)
	file := filepath.Join(native, "state_5.sqlite")
	createdArchivedFlag(t, file, archived)
	if source != "" {
		db := must(sql.Open("sqlite", file))
		_, err := db.Exec("UPDATE threads SET source=?", source)
		check(t, err)
		check(t, db.Close())
	}
	return func(k string) (string, bool) {
		if k == "CODEX_HOME" {
			return native, true
		}
		return env(k)
	}, file
}

// createdArchivedFlag sets the archive flag of every row, as the host does when a child finishes.
func createdArchivedFlag(t *testing.T, file string, archived int) {
	t.Helper()
	db := must(sql.Open("sqlite", file))
	_, err := db.Exec("UPDATE threads SET archived=?", archived)
	check(t, err)
	check(t, db.Close())
}

// A finished child is archived by the host; the thread_spawn marker still proves who created it.
func TestCreatedArchivedNativeCreated(t *testing.T) {
	for _, tc := range []struct {
		name, parent, source, agent string
		archived                    int
		accept                      bool
	}{
		{"archived-own-parent", "session-test", "", "child-a", 1, true},
		{"archived-other-parent", "other", "", "child-a", 1, false},
		{"archived-root-thread", "session-test", `"cli"`, "child-a", 1, false},
		{"archived-invented-id", "session-test", "", "invented", 1, false},
		{"unarchived-own-parent", "session-test", "", "child-a", 0, true},
		{"unarchived-other-parent", "other", "", "child-a", 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ws, env, start, file := dispatchTestFixture(t)
			env, db := createdArchivedHost(t, env, tc.parent, tc.source, tc.archived)
			dispatchTestCall(t, ws, env, map[string]any{"action": "claim", "attemptId": start.AttemptID})
			ledger, hostDB := must(os.ReadFile(file)), must(os.ReadFile(db))
			input := createdCheckInput(start.AttemptID, "created")
			input["agentId"] = tc.agent
			out, err := CheckedDispatch(context.Background(), ws, input, env, nil)
			if tc.accept {
				check(t, err)
				if out.Action != "wait" || out.Attempts[0].AgentID == nil || *out.Attempts[0].AgentID != tc.agent {
					t.Fatalf("created = %+v", out)
				}
			} else {
				if err == nil || !strings.Contains(err.Error(), "copy agentId") {
					t.Fatalf("refusal lacks correction: %v", err)
				}
				if string(ledger) != string(must(os.ReadFile(file))) {
					t.Fatal("refused report changed the ledger")
				}
			}
			if string(hostDB) != string(must(os.ReadFile(db))) {
				t.Fatal("host database changed")
			}
		})
	}
}

// Recovery: the parent lost its created report and the child finished and was archived before the
// parent could record it. Recording the real id must work, and so must the hand-off after it.
func TestCreatedArchivedRecoveryThenClose(t *testing.T) {
	for _, final := range []string{"complete", "stopped"} {
		t.Run(final, func(t *testing.T) {
			ws, env, start, file := dispatchTestFixture(t)
			env, _ = createdArchivedHost(t, env, "session-test", "", 1)
			dispatchTestCall(t, ws, env, map[string]any{"action": "claim", "attemptId": start.AttemptID})
			_, err := CheckedDispatch(context.Background(), ws, createdCheckInput(start.AttemptID, "created"), env, nil)
			check(t, err)
			input := createdCheckInput(start.AttemptID, final)
			if final == "stopped" {
				input["executionState"] = "stopped"
				input["reconciliation"] = "child finished and archived; partial work inspected"
			}
			out, err := CheckedDispatch(context.Background(), ws, input, env, nil)
			check(t, err)
			want := map[string]string{"complete": "complete", "stopped": "stop"}[final]
			if out.Action != want || must(dispatchRead(file, "session-test", "task-test")).Attempts[0].AgentID == nil {
				t.Fatalf("%s = %+v", final, out)
			}
		})
	}
	t.Run("created while live, closed after archive", func(t *testing.T) {
		ws, env, start, file := dispatchTestFixture(t)
		env, db := createdArchivedHost(t, env, "session-test", "", 0)
		dispatchTestCall(t, ws, env, map[string]any{"action": "claim", "attemptId": start.AttemptID})
		_, err := CheckedDispatch(context.Background(), ws, createdCheckInput(start.AttemptID, "created"), env, nil)
		check(t, err)
		createdArchivedFlag(t, db, 1)
		input := createdCheckInput(start.AttemptID, "stopped")
		input["executionState"] = "stopped"
		input["reconciliation"] = "child finished and archived; partial work inspected"
		out, err := CheckedDispatch(context.Background(), ws, input, env, nil)
		check(t, err)
		if stored := must(dispatchRead(file, "session-test", "task-test")); out.Action != "stop" || stored.Status != "stopped" {
			t.Fatalf("close = %+v, stored %q", out, stored.Status)
		}
	})
}

// The App Server reply has no archive field, so an archived thread reads like an unloaded one.
// Each status gets its own ledger: a closed ledger returns before the active check is reached.
func TestCreatedArchivedAppServerParity(t *testing.T) {
	for _, tc := range []struct {
		status string
		closes bool
	}{{"notLoaded", true}, {"active", false}} {
		t.Run(tc.status, func(t *testing.T) {
			ws, env, start, file := dispatchTestFixture(t)
			h := &createdCheckFake{reply: createdCheckReply("session-test", "subagent", tc.status)}
			dispatchTestCall(t, ws, env, map[string]any{"action": "claim", "attemptId": start.AttemptID})
			_, err := CheckedDispatch(context.Background(), ws, createdCheckInput(start.AttemptID, "created"), env, h)
			check(t, err)
			before := must(os.ReadFile(file))
			input := createdCheckInput(start.AttemptID, "stopped")
			input["executionState"] = "stopped"
			input["reconciliation"] = "inspected child"
			out, err := CheckedDispatch(context.Background(), ws, input, env, h)
			switch {
			case tc.closes:
				check(t, err)
				if out.Action != "stop" {
					t.Fatalf("close = %+v", out)
				}
			case err == nil || !strings.Contains(err.Error(), "active") || string(before) != string(must(os.ReadFile(file))):
				t.Fatalf("active child closed or ledger changed: %v", err)
			}
			if h.calls != 2 {
				t.Fatalf("host calls = %d", h.calls)
			}
		})
	}
}

// createdArchivedSession starts and claims a dispatch of the executor role and returns its attempt.
func createdArchivedSession(t *testing.T, ws string, env host.LookupEnv, id string) string {
	t.Helper()
	start := dispatchTestCall(t, ws, env, map[string]any{"action": "start", "role": "executor", "dispatchId": id})
	dispatchTestCall(t, ws, env, map[string]any{"action": "claim", "dispatchId": id, "attemptId": start.AttemptID})
	return start.AttemptID
}

// createdArchivedReport is a created report of one agent for one attempt of one dispatch.
func createdArchivedReport(dispatch, attempt, agent string) map[string]any {
	input := createdCheckInput(attempt, "created")
	input["dispatchId"], input["agentId"] = dispatch, agent
	return input
}

func createdArchivedRecord(ws, id string) string {
	return filepath.Join(ws, ".crw", "dispatches", "session-test", id+".json")
}

// A finished child is archived by the host, but it is still a real child of the session: a later
// dispatch must not be able to report it as the agent that dispatch just spawned.
func TestCreatedArchivedReplayAcrossDispatches(t *testing.T) {
	for _, archived := range []int{1, 0} {
		t.Run(map[int]string{1: "archived", 0: "unarchived"}[archived], func(t *testing.T) {
			ws := t.TempDir()
			env, _ := home(t)
			env, db := createdArchivedHost(t, env, "session-test", "", 0)
			createdCheckSeed(t, filepath.Dir(db), "child-b", "session-test")
			one := createdArchivedSession(t, ws, env, "task-one")
			_, err := CheckedDispatch(context.Background(), ws, createdArchivedReport("task-one", one, "child-a"), env, nil)
			check(t, err)
			dispatchTestCall(t, ws, env, map[string]any{"action": "report", "dispatchId": "task-one", "attemptId": one, "outcome": "complete", "agentId": "child-a"})
			createdArchivedFlag(t, db, archived)
			two := createdArchivedSession(t, ws, env, "task-two")
			first, second := must(os.ReadFile(createdArchivedRecord(ws, "task-one"))), must(os.ReadFile(createdArchivedRecord(ws, "task-two")))
			_, err = CheckedDispatch(context.Background(), ws, createdArchivedReport("task-two", two, "child-a"), env, nil)
			if err == nil || !strings.Contains(err.Error(), "agentId was already reported for dispatch task-one attempt "+one) || !strings.Contains(err.Error(), "copy agentId") {
				t.Fatalf("replay of a finished child: %v", err)
			}
			if string(first) != string(must(os.ReadFile(createdArchivedRecord(ws, "task-one")))) || string(second) != string(must(os.ReadFile(createdArchivedRecord(ws, "task-two")))) {
				t.Fatal("refused replay changed a ledger")
			}
			if temps := must(filepath.Glob(createdArchivedRecord(ws, "*") + ".*.tmp")); len(temps) != 0 {
				t.Fatal("owned temporary remains")
			}
			// A child reported for the first time passes, archived or not: the recovery this issue restores.
			out, err := CheckedDispatch(context.Background(), ws, createdArchivedReport("task-two", two, "child-b"), env, nil)
			check(t, err)
			if out.Action != "wait" {
				t.Fatalf("first report of a real child = %+v", out)
			}
		})
	}
}

// The fallback attempt of one dispatch must not claim the child its stopped first attempt recorded.
func TestCreatedArchivedReplayAcrossAttempts(t *testing.T) {
	ws := t.TempDir()
	env, dir := home(t)
	writeStore(t, dir, `{"roles":{"executor":{"mode":"model","model":"a/one","fallback":{"model":"b/two","effort":null}}}}`)
	env, _ = createdArchivedHost(t, env, "session-test", "", 1)
	first := createdArchivedSession(t, ws, env, "task-one")
	_, err := CheckedDispatch(context.Background(), ws, createdArchivedReport("task-one", first, "child-a"), env, nil)
	check(t, err)
	next := dispatchTestCall(t, ws, env, map[string]any{"action": "report", "dispatchId": "task-one", "attemptId": first, "outcome": "failed", "error": "insufficient_quota", "executionState": "stopped", "agentId": "child-a", "reconciliation": "first child stopped; partial work inspected"})
	if next.AttemptID == first || len(next.Attempts) != 2 {
		t.Fatalf("no fallback attempt: %+v", next)
	}
	dispatchTestCall(t, ws, env, map[string]any{"action": "claim", "dispatchId": "task-one", "attemptId": next.AttemptID})
	before := must(os.ReadFile(createdArchivedRecord(ws, "task-one")))
	_, err = CheckedDispatch(context.Background(), ws, createdArchivedReport("task-one", next.AttemptID, "child-a"), env, nil)
	if err == nil || !strings.Contains(err.Error(), "agentId was already reported for dispatch task-one attempt "+first) {
		t.Fatalf("replay by the fallback attempt: %v", err)
	}
	if string(before) != string(must(os.ReadFile(createdArchivedRecord(ws, "task-one")))) {
		t.Fatal("refused replay changed the ledger")
	}
}

// The ledger lets one attempt report the same agent again (only a different id is "agentId changed").
func TestCreatedArchivedSameAttemptRepeat(t *testing.T) {
	ws := t.TempDir()
	env, _ := home(t)
	env, _ = createdArchivedHost(t, env, "session-test", "", 1)
	one := createdArchivedSession(t, ws, env, "task-one")
	for i := 0; i < 2; i++ {
		out, err := CheckedDispatch(context.Background(), ws, createdArchivedReport("task-one", one, "child-a"), env, nil)
		check(t, err)
		if out.Action != "wait" || *out.Attempts[0].AgentID != "child-a" {
			t.Fatalf("report %d = %+v", i, out)
		}
	}
}

// The ledger alone refutes a replay, so the host is not asked.
func TestCreatedArchivedReplayMakesNoHostCall(t *testing.T) {
	ws := t.TempDir()
	env, _ := home(t)
	h := &createdCheckFake{reply: createdCheckReply("session-test", "subagent", "notLoaded")}
	one := createdArchivedSession(t, ws, env, "task-one")
	_, err := CheckedDispatch(context.Background(), ws, createdArchivedReport("task-one", one, "child-a"), env, h)
	check(t, err)
	two := createdArchivedSession(t, ws, env, "task-two")
	_, err = CheckedDispatch(context.Background(), ws, createdArchivedReport("task-two", two, "child-a"), env, h)
	if err == nil || !strings.Contains(err.Error(), "already reported for dispatch task-one") || h.calls != 1 {
		t.Fatalf("replay: %v after %d host calls", err, h.calls)
	}
}

// A record the guard cannot read means it cannot tell whether the id was reported, so it refuses;
// lock directories and temporary files are not records.
func TestCreatedArchivedSiblingRecords(t *testing.T) {
	for _, tc := range []struct {
		name   string
		plant  func(t *testing.T, path string)
		accept bool
	}{
		{"corrupt record", func(t *testing.T, path string) { check(t, os.WriteFile(path, []byte("{"), 0o600)) }, false},
		{"symlinked record", func(t *testing.T, path string) { check(t, os.Symlink(path+"-target", path)) }, false},
		{"directory named like a record", func(t *testing.T, path string) { check(t, os.Mkdir(path, 0o700)) }, false},
		{"named pipe named like a record", func(t *testing.T, path string) { check(t, syscall.Mkfifo(path, 0o600)) }, false},
		{"stale lock directory", func(t *testing.T, path string) { check(t, os.Mkdir(path+".lock", 0o700)) }, true},
		{"abandoned temporary file", func(t *testing.T, path string) { check(t, os.WriteFile(path+".0f0f.tmp", []byte("{"), 0o600)) }, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ws := t.TempDir()
			env, _ := home(t)
			env, _ = createdArchivedHost(t, env, "session-test", "", 1)
			one := createdArchivedSession(t, ws, env, "task-one")
			tc.plant(t, createdArchivedRecord(ws, "stray"))
			before := must(os.ReadFile(createdArchivedRecord(ws, "task-one")))
			done := make(chan error, 1)
			go func() {
				_, err := CheckedDispatch(context.Background(), ws, createdArchivedReport("task-one", one, "child-a"), env, nil)
				done <- err
			}()
			select {
			case err := <-done:
				if tc.accept {
					check(t, err)
				} else if err == nil {
					t.Fatal("unreadable sibling record accepted")
				} else if string(before) != string(must(os.ReadFile(createdArchivedRecord(ws, "task-one")))) {
					t.Fatal("refused report changed the ledger")
				}
			case <-time.After(10 * time.Second):
				// Release a reader that is blocked on a named pipe, so a failure leaves no goroutine behind.
				if f, err := os.OpenFile(createdArchivedRecord(ws, "stray"), os.O_RDWR, 0); err == nil {
					_ = f.Close()
				}
				t.Fatal("the guard blocked on a sibling record")
			}
		})
	}
}
