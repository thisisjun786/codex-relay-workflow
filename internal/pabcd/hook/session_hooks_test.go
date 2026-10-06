package hook

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/dev/cxccorpus"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/crwdir"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/host"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/interview"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/interview/ledger"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/state"
	_ "modernc.org/sqlite"
)

// sessionHookEnv is a lookup over a fixed map, so a test states the host it runs against.
func sessionHookEnv(vars map[string]string) host.LookupEnv {
	return func(key string) (string, bool) { v, ok := vars[key]; return v, ok }
}

// TestSessionHookPostCompactKeepsAParticipatingWritersUpdate is the data-loss fix: an update that
// lands between the handler's read and its write is kept, because the file is read again inside the
// lock. The seam stands in for the writer, which the handler cannot be timed against.
func TestSessionHookPostCompactKeepsAParticipatingWritersUpdate(t *testing.T) {
	cwd := t.TempDir()
	phase := state.PhaseB
	sessionHookStateFile(t, cwd, "pc3", func(s *state.State) {
		s.Phase, s.OrchestrationActive, s.LastInjectedPhase = state.PhaseB, true, &phase
	})
	writer := func(cwd, sessionID string, fn func() error) error {
		s := state.ReadState(cwd, sessionID)
		s.MemoryWriteGrant = true
		if err := state.WriteState(cwd, s); err != nil {
			return err
		}
		return state.WithSessionLock(cwd, sessionID, fn)
	}
	if answer := sessionHookPostCompact(SessionHookPostCompactPayload{Cwd: cwd, SessionID: "pc3"}, writer); answer != "" {
		t.Errorf("answer %q", answer)
	}
	if s := state.ReadState(cwd, "pc3"); !s.MemoryWriteGrant || s.LastInjectedPhase != nil {
		t.Errorf("the participating writer's update was lost: %+v", s)
	}
}

// TestSessionHookPostCompactRefusesALossyOrLockedState is the other half of the fix: a state whose
// stored records the reader cannot keep is not rewritten from a lossy read, and a lock that cannot be
// taken leaves the file as it was.
func TestSessionHookPostCompactRefusesALossyOrLockedState(t *testing.T) {
	cwd := t.TempDir()
	phase := state.PhaseB
	tracker := func(n int) *interview.Tracker {
		t := &interview.Tracker{Contradictions: []interview.Contradiction{}, Assumptions: []interview.Assumption{}}
		for i := 0; i < n; i++ {
			t.Contradictions = append(t.Contradictions, interview.Contradiction{ContradictionID: fmt.Sprintf("c%d", i), Summary: "s", Severity: interview.SeverityHigh})
		}
		return t
	}
	lossy := sessionHookStateFile(t, cwd, "pc4", func(s *state.State) {
		s.Phase, s.OrchestrationActive, s.LastInjectedPhase = state.PhaseB, true, &phase
		for i := 0; i <= state.MaxUnverifiedSubagents; i++ { // one past the cap: the reader would drop the last
			s.UnverifiedSubagents = append(s.UnverifiedSubagents, state.UnverifiedSubagent{AgentID: fmt.Sprintf("a%d", i),
				TurnID: "t", AgentType: "worker", Attempts: 1, ReceiptClaimed: "verdict", RecordedAt: "2026-01-01T00:00:00.000Z", Resolvable: true})
		}
	})
	before, err := os.ReadFile(lossy)
	if err != nil {
		t.Fatal(err)
	}
	SessionHookPostCompact(SessionHookPostCompactPayload{Cwd: cwd, SessionID: "pc4"})
	if after, err := os.ReadFile(lossy); err != nil || string(after) != string(before) {
		t.Errorf("a lossy state was rewritten: %v", err)
	}
	// A stored interview tracker one past the reader's cap is not shortened by a write that only clears the cursor.
	capped := sessionHookStateFile(t, cwd, "pc6", func(s *state.State) {
		s.Phase, s.OrchestrationActive, s.LastInjectedPhase = state.PhaseB, true, &phase
		s.Interview = tracker(interview.MaxTrackerArray + 1)
	})
	before, err = os.ReadFile(capped)
	if err != nil {
		t.Fatal(err)
	}
	SessionHookPostCompact(SessionHookPostCompactPayload{Cwd: cwd, SessionID: "pc6"})
	if after, err := os.ReadFile(capped); err != nil || string(after) != string(before) {
		t.Errorf("a capped interview tracker was shortened: %v", err)
	}
	// A tracker within the cap round-trips, so the reset still happens and the tracker is kept.
	sessionHookStateFile(t, cwd, "pc7", func(s *state.State) {
		s.Phase, s.OrchestrationActive, s.LastInjectedPhase = state.PhaseB, true, &phase
		s.Interview = tracker(2)
	})
	SessionHookPostCompact(SessionHookPostCompactPayload{Cwd: cwd, SessionID: "pc7"})
	if s := state.ReadState(cwd, "pc7"); s.LastInjectedPhase != nil || s.Interview == nil || len(s.Interview.Contradictions) != 2 {
		t.Errorf("a within-cap tracker: %+v", s)
	}
	locked := sessionHookStateFile(t, cwd, "pc5", func(s *state.State) {
		s.Phase, s.OrchestrationActive, s.LastInjectedPhase = state.PhaseB, true, &phase
	})
	before, err = os.ReadFile(locked)
	if err != nil {
		t.Fatal(err)
	}
	held := func(cwd, sessionID string, fn func() error) error { return errors.New("a held lock") }
	sessionHookPostCompact(SessionHookPostCompactPayload{Cwd: cwd, SessionID: "pc5"}, held)
	if after, err := os.ReadFile(locked); err != nil || string(after) != string(before) {
		t.Errorf("a held lock wrote the file: %v", err)
	}
}

// TestSessionHookRescanDirectiveMatchesTheK5Extraction pins the ported constant to the oracle's
// exported RESCAN_REINJECT_DIRECTIVE after the declared name substitution (contract K5), byte for
// byte: the text a hook injects is the behaviour the model acts on, so it is a contract here.
func TestSessionHookRescanDirectiveMatchesTheK5Extraction(t *testing.T) {
	var extraction struct {
		Modules map[string]map[string]json.RawMessage
	}
	raw, err := os.ReadFile("../../../contract/schema/cxc/injected-text.json")
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &extraction); err != nil {
		t.Fatal(err)
	}
	sub, err := cxccorpus.LoadSubstitution("../../..")
	if err != nil {
		t.Fatal(err)
	}
	value, ok := extraction.Modules["components/pabcd-state/dist/hook.js"]["RESCAN_REINJECT_DIRECTIVE"]
	if !ok {
		t.Fatal("the K5 extraction has no RESCAN_REINJECT_DIRECTIVE")
	}
	var oracle string
	if err := json.Unmarshal(value, &oracle); err != nil {
		t.Fatal(err)
	}
	if want := sub.Expected(oracle); sessionHookRescanReinjectDirective != want {
		t.Errorf("rescan directive:\n got %q\nwant %q", sessionHookRescanReinjectDirective, want)
	}
}

// sessionHookWriteState writes one state file directly, so a test can pin updatedAt and tell a
// no-op from a write that changed only the timestamp.
func sessionHookWriteState(t *testing.T, cwd, sessionID string, s state.State) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(cwd, crwdir.DirName, state.SessionsSubdir), 0o755); err != nil {
		t.Fatal(err)
	}
	body, err := state.Encode(s)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(state.StatePath(cwd, sessionID), body, 0o666); err != nil {
		t.Fatal(err)
	}
}

// sessionHookStateFile builds a state from the reader's own default and pins its updatedAt.
func sessionHookStateFile(t *testing.T, cwd, sessionID string, mutate func(*state.State)) string {
	t.Helper()
	if _, err := state.EnsureState(cwd, sessionID); err != nil {
		t.Fatal(err)
	}
	s := state.ReadState(cwd, sessionID)
	mutate(&s)
	s.UpdatedAt = "2000-01-01T00:00:00.000Z"
	sessionHookWriteState(t, cwd, sessionID, s)
	return state.StatePath(cwd, sessionID)
}

// TestSessionHookSessionStartCreatesTheStateAndIgnore is state.test.ts "issue255: SessionStart
// creates state and exact local ignore file" as a Go test: the bootstrap leg writes the session's
// IDLE state and the state directory's .gitignore, and answers nothing.
func TestSessionHookSessionStartCreatesTheStateAndIgnore(t *testing.T) {
	cwd := t.TempDir()
	if answer := SessionHookSessionStart(SessionHookSessionStartPayload{Cwd: cwd, SessionID: "issue255"}); answer != "" {
		t.Errorf("answer %q", answer)
	}
	if _, err := os.Stat(state.StatePath(cwd, "issue255")); err != nil {
		t.Errorf("no state file: %v", err)
	}
	ignore, err := os.ReadFile(filepath.Join(cwd, crwdir.DirName, ".gitignore"))
	if err != nil || string(ignore) != crwdir.GitignoreText {
		t.Errorf(".gitignore %q: %v", ignore, err)
	}
	// A resumed session's file is left byte for byte unchanged, corrupt content included.
	path := sessionHookStateFile(t, cwd, "issue255", func(s *state.State) { s.Phase, s.OrchestrationActive = state.PhaseP, true })
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	SessionHookSessionStart(SessionHookSessionStartPayload{Cwd: cwd, SessionID: "issue255"})
	after, err := os.ReadFile(path)
	if err != nil || string(after) != string(before) {
		t.Errorf("a resumed state changed: %q, %v", after, err)
	}
	corrupt := filepath.Join(cwd, crwdir.DirName, state.SessionsSubdir, "broken.json")
	if err := os.WriteFile(corrupt, []byte("{not json"), 0o666); err != nil {
		t.Fatal(err)
	}
	SessionHookSessionStart(SessionHookSessionStartPayload{Cwd: cwd, SessionID: "broken"})
	if data, err := os.ReadFile(corrupt); err != nil || string(data) != "{not json" {
		t.Errorf("a corrupt state was repaired: %q, %v", data, err)
	}
}

// TestSessionHookPostCompactResetsOnlyTheCursor is hook-continuation.test.ts "050" as a Go test: an
// in-flight cycle has its reinjection cursor cleared, everything else is untouched, and an idle or
// already-reset session is not written at all.
func TestSessionHookPostCompactResetsOnlyTheCursor(t *testing.T) {
	cwd := t.TempDir()
	active := func(s *state.State) {
		phase := state.PhaseB
		s.Phase, s.OrchestrationActive, s.LastInjectedPhase = state.PhaseB, true, &phase
		s.StopBlockPhase, s.StopBlockCount = &phase, 2
	}
	path := sessionHookStateFile(t, cwd, "pc1", active)
	if answer := SessionHookPostCompact(SessionHookPostCompactPayload{Cwd: cwd, SessionID: "pc1"}); answer != "" {
		t.Errorf("answer %q", answer)
	}
	s := state.ReadState(cwd, "pc1")
	if s.LastInjectedPhase != nil || s.Phase != state.PhaseB || !s.OrchestrationActive || s.StopBlockCount != 2 {
		t.Errorf("state after the reset: %+v", s)
	}
	// A second compaction finds the cursor already reset and writes nothing.
	reset, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	SessionHookPostCompact(SessionHookPostCompactPayload{Cwd: cwd, SessionID: "pc1"})
	if again, err := os.ReadFile(path); err != nil || string(again) != string(reset) {
		t.Errorf("an already-reset cursor was rewritten: %q, %v", again, err)
	}
	// An idle session has nothing to recover.
	idle := sessionHookStateFile(t, cwd, "pc2", func(s *state.State) { s.Phase, s.OrchestrationActive = state.PhaseIdle, false })
	before, err := os.ReadFile(idle)
	if err != nil {
		t.Fatal(err)
	}
	SessionHookPostCompact(SessionHookPostCompactPayload{Cwd: cwd, SessionID: "pc2"})
	if after, err := os.ReadFile(idle); err != nil || string(after) != string(before) {
		t.Errorf("an idle state was written: %q, %v", after, err)
	}
}

func sessionHookRound() (map[string]any, map[string]any) {
	return map[string]any{"questions": []any{map[string]any{"id": "q1", "header": "Scope", "question": "Which scope?"}}},
		map[string]any{"answers": map[string]any{"q1": map[string]any{"answers": []any{"Small"}}}}
}

// sessionHookAnswerContext is the additionalContext of the leg's answer, which must be one
// hookSpecificOutput line.
func sessionHookAnswerContext(t *testing.T, answer string) string {
	t.Helper()
	var out struct {
		Specific struct {
			Event   string `json:"hookEventName"`
			Context string `json:"additionalContext"`
		} `json:"hookSpecificOutput"`
	}
	if err := json.Unmarshal([]byte(answer), &out); err != nil || out.Specific.Event != "PostToolUse" {
		t.Fatalf("not the PostToolUse envelope: %q (%v)", answer, err)
	}
	return out.Specific.Context
}

// sessionHookGoalsDB builds the host's goals database at home from the given statements and returns
// home, which is what CODEX_SQLITE_HOME is set to.
func sessionHookGoalsDB(t *testing.T, home string, statements ...string) string {
	t.Helper()
	if err := os.MkdirAll(home, 0o755); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", filepath.Join(home, host.GoalsDBFilename))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, statement := range statements {
		if _, err := db.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	return home
}

// TestSessionHookPostToolUseCapturesAndReinjects is interview-ledger.test.ts's handlePostToolUse
// cases as a Go test: the round is recorded for request_user_input only, and the post-answer rescan
// directive is reinjected only in an interactive I phase.
func TestSessionHookPostToolUseCapturesAndReinjects(t *testing.T) {
	dir := t.TempDir()
	cwd := filepath.Join(dir, "ws")
	if err := os.MkdirAll(cwd, 0o755); err != nil {
		t.Fatal(err)
	}
	env := sessionHookEnv(map[string]string{"CODEX_SQLITE_HOME": filepath.Join(dir, "codex")})
	input, response := sessionHookRound()
	call := func(sessionID, turn, tool string) string {
		return SessionHookPostToolUse(SessionHookPostToolUsePayload{Cwd: cwd, SessionID: sessionID, ToolName: tool,
			TurnID: turn, ToolInput: input, ToolResponse: response}, env)
	}

	// No session state reads as IDLE: capture only, no reinjection.
	if answer := call("s1", "t1", "request_user_input"); answer != "" {
		t.Errorf("an IDLE session reinjected: %q", answer)
	}
	if rows := ledger.ReadQaEvents(cwd, "s1"); len(rows) != 2 || rows[0].Event != ledger.QuestionAsked || rows[1].Event != ledger.AnswerRecorded {
		t.Fatalf("captured rows: %+v", rows)
	}
	// Another tool is a no-op.
	if answer := call("s2", "t1", "view_image"); answer != "" || len(ledger.ReadQaEvents(cwd, "s2")) != 0 {
		t.Errorf("another tool: %q, %+v", answer, ledger.ReadQaEvents(cwd, "s2"))
	}
	// An interactive I phase reinjects the rescan directive, with its command resolved at emission.
	sessionHookStateFile(t, cwd, "s3", func(s *state.State) { s.Phase, s.OrchestrationActive = state.PhaseI, true })
	resolving := sessionHookEnv(map[string]string{"CODEX_SQLITE_HOME": filepath.Join(dir, "codex"), "CRW_BIN": "{CRW}"})
	answer := SessionHookPostToolUse(SessionHookPostToolUsePayload{Cwd: cwd, SessionID: "s3", ToolName: "request_user_input",
		TurnID: "t1", ToolInput: input, ToolResponse: response}, resolving)
	resolved := ResolveCRWInDirective(sessionHookRescanReinjectDirective, resolving)
	if got := sessionHookAnswerContext(t, answer); got != resolved {
		t.Errorf("reinjected:\n got %q\nwant %q", got, resolved)
	}
	if rows := ledger.ReadQaEvents(cwd, "s3"); len(rows) != 2 {
		t.Errorf("the round beside the reinjection: %+v", rows)
	}
	// A goal that suppresses the interview captures silently (the goal firewall): an active row, and
	// a database whose row cannot be read at all (fail closed).
	for _, c := range []struct {
		sessionID string
		home      string
	}{
		{"s4", sessionHookGoalsDB(t, filepath.Join(dir, "codex-active"),
			"CREATE TABLE thread_goals (thread_id TEXT PRIMARY KEY, status TEXT)", "INSERT INTO thread_goals VALUES ('s4', 'active')")},
		{"s5", sessionHookGoalsDB(t, filepath.Join(dir, "codex-unreadable"),
			"CREATE TABLE thread_goals (thread_id TEXT PRIMARY KEY)")},
	} {
		sessionHookStateFile(t, cwd, c.sessionID, func(s *state.State) { s.Phase, s.OrchestrationActive = state.PhaseI, true })
		answer := SessionHookPostToolUse(SessionHookPostToolUsePayload{Cwd: cwd, SessionID: c.sessionID, ToolName: "request_user_input",
			TurnID: "t1", ToolInput: input, ToolResponse: response}, sessionHookEnv(map[string]string{"CODEX_SQLITE_HOME": c.home}))
		if answer != "" {
			t.Errorf("%s: the interview was not suppressed: %q", c.sessionID, answer)
		}
		if rows := ledger.ReadQaEvents(cwd, c.sessionID); len(rows) != 2 {
			t.Errorf("%s: capture did not run: %+v", c.sessionID, rows)
		}
	}
}
