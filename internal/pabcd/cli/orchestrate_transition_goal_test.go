package cli

import (
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/host"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/state"
)

// orchestrateGoalWorld is orchestrateTransitionRoot with the host goals database pointed at a temporary directory
// (CODEX_SQLITE_HOME) that holds status for id: "" builds no database, "garbage" a file that is not one.
func orchestrateGoalWorld(t *testing.T, id, status string) string {
	t.Helper()
	cwd := orchestrateTransitionRoot(t)
	dir := t.TempDir()
	t.Setenv("CODEX_SQLITE_HOME", dir)
	path := filepath.Join(dir, host.GoalsDBFilename)
	switch status {
	case "":
	case "garbage":
		if err := os.WriteFile(path, []byte("this is not a sqlite database, only text padding it out to some length"), 0o600); err != nil {
			t.Fatal(err)
		}
	default:
		db, err := sql.Open("sqlite", path)
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		for _, statement := range []string{
			"CREATE TABLE thread_goals (thread_id TEXT PRIMARY KEY NOT NULL, status TEXT NOT NULL, objective TEXT)",
			"INSERT INTO thread_goals (thread_id, status, objective) VALUES ('" + id + "', '" + status + "', 'ship the feature')",
			"INSERT INTO thread_goals (thread_id, status, objective) VALUES ('another-thread', 'active', 'not this one')",
		} {
			if _, err := db.Exec(statement); err != nil {
				t.Fatal(err)
			}
		}
	}
	return cwd
}

// CRW-1179: the host goal firewall holds on the CLI entry to Interview, not only on the UserPromptSubmit hint.
func TestOrchestrateTransitionRefusesInterviewUnderAnActiveGoal(t *testing.T) {
	t.Run("idle-to-I-active-goal", func(t *testing.T) {
		id := "goal-idle-i"
		cwd := orchestrateGoalWorld(t, id, "active")
		orchestrateTransitionSession(t, cwd, id, `{"phase":"IDLE"}`)
		before, err := os.ReadFile(state.StatePath(cwd, id))
		if err != nil {
			t.Fatal(err)
		}
		got := orchestrateTransitionRun(t, cwd, "I", "--session", id)
		if got.Code != 1 || !strings.HasPrefix(got.Output, "orchestrate I: current=IDLE session="+id+"; ") ||
			!strings.Contains(got.Output, "active host goal") ||
			!strings.Contains(got.Output, "crw pabcd orchestrate P --session "+id) ||
			!strings.Contains(got.Output, "Nothing was written.") {
			t.Fatalf("refusal: %+v", got)
		}
		after, err := os.ReadFile(state.StatePath(cwd, id))
		if err != nil || string(after) != string(before) || len(orchestrateTransitionLedger(t, cwd)) != 0 {
			t.Fatalf("the refusal wrote state or a row: %v", err)
		}
	})
	t.Run("mid-cycle-to-I-active-goal", func(t *testing.T) {
		for _, phase := range []string{"P", "A", "B", "C"} {
			id := "goal-" + phase + "-i"
			cwd := orchestrateGoalWorld(t, id, "active")
			orchestrateTransitionSession(t, cwd, id, `{"phase":"`+phase+`","orchestrationActive":true}`)
			got := orchestrateTransitionRun(t, cwd, "I", "--session", id)
			if got.Code != 1 || !strings.Contains(got.Output, "active host goal") ||
				!strings.Contains(got.Output, "Continue the "+phase+" phase") || strings.Contains(got.Output, "orchestrate P --session") {
				t.Fatalf("%s>I: %+v", phase, got)
			}
			if state.ReadState(cwd, id).Phase != state.Phase(phase) || len(orchestrateTransitionLedger(t, cwd)) != 0 {
				t.Fatalf("%s>I: the refusal wrote state or a row", phase)
			}
		}
	})
	t.Run("unreadable-database-fails-closed", func(t *testing.T) {
		id := "goal-unreadable"
		cwd := orchestrateGoalWorld(t, id, "garbage")
		orchestrateTransitionSession(t, cwd, id, `{"phase":"IDLE"}`)
		got := orchestrateTransitionRun(t, cwd, "I", "--session", id)
		if got.Code != 1 || !strings.Contains(got.Output, "cannot be read") || !strings.Contains(got.Output, "crw pabcd orchestrate P --session "+id) {
			t.Fatalf("unreadable: %+v", got)
		}
		if state.ReadState(cwd, id).Phase != state.PhaseIdle {
			t.Fatal("the fail-closed refusal wrote state")
		}
	})
	t.Run("no-goal-enters-I", func(t *testing.T) {
		for name, status := range map[string]string{"no database": "", "paused": "paused", "complete": "complete"} {
			id := "goal-none"
			cwd := orchestrateGoalWorld(t, id, status)
			orchestrateTransitionSession(t, cwd, id, `{"phase":"IDLE"}`)
			got := orchestrateTransitionRun(t, cwd, "I", "--session", id)
			if got.Code != 0 || state.ReadState(cwd, id).Phase != state.PhaseI {
				t.Fatalf("%s: %+v", name, got)
			}
			if rows := orchestrateTransitionLedger(t, cwd); len(rows) != 1 || rows[0]["from"] != "IDLE" || rows[0]["to"] != "I" {
				t.Fatalf("%s: ledger %+v", name, rows)
			}
		}
	})
	t.Run("a-goal-of-another-thread-does-not-refuse", func(t *testing.T) {
		id := "goal-mine"
		cwd := orchestrateGoalWorld(t, "someone-else", "active")
		orchestrateTransitionSession(t, cwd, id, `{"phase":"IDLE"}`)
		if got := orchestrateTransitionRun(t, cwd, "I", "--session", id); got.Code != 0 {
			t.Fatalf("other thread's goal refused this session: %+v", got)
		}
	})
	t.Run("leaving-I-stays-open-under-a-goal", func(t *testing.T) {
		// A goal created while the interview was open must not trap the session in I: P (override) and IDLE remain.
		id := "goal-leave-i"
		cwd := orchestrateGoalWorld(t, id, "active")
		orchestrateTransitionSession(t, cwd, id, `{"phase":"I","orchestrationActive":true}`)
		got := orchestrateTransitionRun(t, cwd, "P", "--session", id, "--attest", `{"from":"I","to":"P","did":"goal started mid-interview; planning now","override":true}`)
		if got.Code != 0 || state.ReadState(cwd, id).Phase != state.PhaseP {
			t.Fatalf("I>P under a goal: %+v", got)
		}
		id = "goal-leave-i-reset"
		orchestrateTransitionSession(t, cwd, id, `{"phase":"I","orchestrationActive":true}`)
		if got := orchestrateTransitionRun(t, cwd, "reset", "--session", id); got.Code != 0 || state.ReadState(cwd, id).Phase != state.PhaseIdle {
			t.Fatalf("reset from I under a goal: %+v", got)
		}
	})
}
