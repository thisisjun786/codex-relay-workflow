package role

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"

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
	db := must(sql.Open("sqlite", file))
	_, err := db.Exec("UPDATE threads SET archived=?", archived)
	check(t, err)
	if source != "" {
		_, err = db.Exec("UPDATE threads SET source=?", source)
		check(t, err)
	}
	check(t, db.Close())
	return func(k string) (string, bool) {
		if k == "CODEX_HOME" {
			return native, true
		}
		return env(k)
	}, file
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
		env, _ = createdArchivedHost(t, env, "session-test", "", 1)
		dispatchTestCall(t, ws, env, map[string]any{"action": "claim", "attemptId": start.AttemptID})
		dispatchTestCall(t, ws, env, map[string]any{"action": "report", "attemptId": start.AttemptID, "outcome": "created", "agentId": "child-a"})
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
