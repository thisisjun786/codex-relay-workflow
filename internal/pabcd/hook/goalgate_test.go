package hook

import (
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	_ "modernc.org/sqlite" // the pure-Go driver the repository already uses: no CGO

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/host"
)

// The budget and interview cases of CXC v0.2.40 pabcd-state/test/goal-gate.test.ts (3c1459ac) as Go tests. The
// goal-complete cases belong to CRW-752, which ports that guard's body.
//
// Every test runs against temporary homes: the environment is an explicit lookup over a map, and the three home
// variables are pointed into t.TempDir() before anything else, so no case can read or write the real ~/.codex,
// ~/.crw or ~/.codexclaw.

// goalGateTestEnv is the environment the gate reads: three temporary homes, with CODEX_SQLITE_HOME left to the
// caller. The returned map is the one the lookup reads, so a case sets a variable by assigning to it.
func goalGateTestEnv(t *testing.T) (map[string]string, host.LookupEnv) {
	t.Helper()
	dir := t.TempDir()
	vars := map[string]string{
		"HOME":       filepath.Join(dir, "home"),
		"CODEX_HOME": filepath.Join(dir, "codex"),
		"CRW_HOME":   filepath.Join(dir, "crw"),
	}
	for key, path := range vars {
		if err := os.MkdirAll(path, 0o755); err != nil {
			t.Fatal(err)
		}
		t.Setenv(key, path) // the real variables are replaced, not merely ignored
	}
	return vars, func(key string) (string, bool) { value, ok := vars[key]; return value, ok }
}

// goalGateTestGoalsDB seeds the host's goals database under dir and returns dir, which a case then binds to
// CODEX_SQLITE_HOME.
func goalGateTestGoalsDB(t *testing.T, dir string, statements ...string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", filepath.Join(dir, host.GoalsDBFilename))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, statement := range statements {
		if _, err := db.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// goalGateTestPayload is a PreToolUse payload as codex-rs writes it.
func goalGateTestPayload(t *testing.T, cwd, sessionID, tool string, input any) string {
	t.Helper()
	raw, err := json.Marshal(map[string]any{"hook_event_name": "PreToolUse", "session_id": sessionID,
		"cwd": cwd, "tool_name": tool, "tool_input": input})
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// goalGateTestDeny reads a deny envelope, which must be one hookSpecificOutput line for PreToolUse ending in a
// newline, and returns its reason and its additional context.
func goalGateTestDeny(t *testing.T, answer string) (reason, context string) {
	t.Helper()
	if !strings.HasSuffix(answer, "\n") {
		t.Fatalf("the envelope has no trailing newline: %q", answer)
	}
	raw := map[string]any{}
	if err := json.Unmarshal([]byte(answer), &raw); err != nil {
		t.Fatal(err)
	}
	specific, _ := raw["hookSpecificOutput"].(map[string]any)
	if len(raw) != 1 || specific == nil {
		t.Fatalf("not one hookSpecificOutput object: %q", answer)
	}
	event, _ := specific["hookEventName"].(string)
	decision, _ := specific["permissionDecision"].(string)
	if event != "PreToolUse" || decision != "deny" {
		t.Fatalf("the envelope names another event or decision: %q", answer)
	}
	if len(specific) != 4 {
		t.Fatalf("the envelope carries %d keys, not the four the oracle writes: %q", len(specific), answer)
	}
	return goalGateTestString(specific["permissionDecisionReason"]), goalGateTestString(specific["additionalContext"])
}

func goalGateTestString(value any) string {
	s, _ := value.(string)
	return s
}

// goal-gate.test.ts:41-83 — the budget guard.
func TestGoalGateBudgetGuard(t *testing.T) {
	cwd := t.TempDir()
	guard := func(tool string, input any) string {
		return goalGateApplyGoalBudgetGuard(goalGatePreToolUse{Cwd: cwd, SessionID: "s1", ToolName: tool, ToolInput: input})
	}
	if out := guard("create_goal", map[string]any{"objective": "do x"}); out != "" {
		t.Errorf("an objective-only create_goal is denied: %q", out)
	}
	if out := guard("shell", map[string]any{"command": "ls", "token_budget": 5}); out != "" {
		t.Errorf("another tool is denied: %q", out)
	}
	if out := guard("create_goal", map[string]any{}); out != "" {
		t.Errorf("an empty input is denied: %q", out)
	}
	if out := guard("create_goal", nil); out != "" {
		t.Errorf("a null input is denied: %q", out)
	}
	if out := guard("create_goal", "objective"); out != "" {
		t.Errorf("a string input is denied: %q", out)
	}
	budget := guard("create_goal", map[string]any{"objective": "do x", "token_budget": 1000})
	extra := guard("create_goal", map[string]any{"objective": "do x", "foo": 1})
	if budget == "" || extra == "" {
		t.Fatalf("a create_goal with an extra key passes: %q, %q", budget, extra)
	}
	if budget != extra {
		t.Errorf("the extra-key envelope differs from the token_budget one:\n%s\n%s", budget, extra)
	}
	reason, context := goalGateTestDeny(t, budget)
	if reason != goalGateCreateGoalWarning || context != reason {
		t.Errorf("the budget envelope: reason %q, context %q", reason, context)
	}
	if !strings.Contains(reason, "token_budget") {
		t.Errorf("the reason does not name token_budget: %q", reason)
	}
}

// goal-gate.test.ts:85-115 — the parse, and the parse-then-guard path.
func TestGoalGateParsePreToolUse(t *testing.T) {
	raw := goalGateTestPayload(t, "/tmp/x", "s1", "create_goal", map[string]any{"objective": "y", "token_budget": 9})
	p, ok := goalGateParsePreToolUse(raw)
	if !ok || p.ToolName != "create_goal" || p.SessionID != "s1" || p.Cwd != "/tmp/x" {
		t.Fatalf("a valid payload: %+v, %v", p, ok)
	}
	if out := goalGateApplyGoalBudgetGuard(p); out == "" {
		t.Error("a budgeted create_goal is not denied after the parse")
	}
	object := func(fields map[string]any) string {
		raw, err := json.Marshal(fields)
		if err != nil {
			t.Fatal(err)
		}
		return string(raw)
	}
	for name, bad := range map[string]string{
		"empty":         "",
		"whitespace":    "   ",
		"unparseable":   "{ not json",
		"another event": object(map[string]any{"hook_event_name": "Stop", "session_id": "s", "cwd": "/t", "tool_name": "create_goal"}),
		"no event":      object(map[string]any{"session_id": "s", "cwd": "/t", "tool_name": "create_goal"}),
		"no session":    object(map[string]any{"hook_event_name": "PreToolUse", "cwd": "/t", "tool_name": "create_goal"}),
		"no cwd":        object(map[string]any{"hook_event_name": "PreToolUse", "session_id": "s", "tool_name": "create_goal"}),
		"no tool":       object(map[string]any{"hook_event_name": "PreToolUse", "session_id": "s", "cwd": "/t"}),
		"a mistyped id": object(map[string]any{"hook_event_name": "PreToolUse", "session_id": 7, "cwd": "/t", "tool_name": "create_goal"}),
		"an array":      "[]",
		"a scalar":      "\"objective\"",
		"null":          "null",
	} {
		if _, ok := goalGateParsePreToolUse(bad); ok {
			t.Errorf("%s parsed as a payload: %q", name, bad)
		}
	}
}

// goal-gate.test.ts:145-180 — the interview guard, whose status is read only for request_user_input.
func TestGoalGateInterviewGuard(t *testing.T) {
	vars, env := goalGateTestEnv(t)
	dir := t.TempDir()
	vars["CODEX_SQLITE_HOME"] = goalGateTestGoalsDB(t, filepath.Join(dir, "active"),
		"CREATE TABLE thread_goals (thread_id TEXT PRIMARY KEY NOT NULL, status TEXT NOT NULL, objective TEXT)",
		"INSERT INTO thread_goals VALUES ('s1', 'active', 'ship')")
	guard := func(tool string) string {
		return goalGateApplyGoalModeInterviewGuard(goalGatePreToolUse{SessionID: "s1", ToolName: tool},
			func() host.GoalStatus { return sessionHookGoalStatus("s1", env) })
	}
	reason, context := goalGateTestDeny(t, guard("request_user_input"))
	if reason != goalGateModeDenyReason || context != goalGateModeDenyReason+" (goal-active=active)" {
		t.Errorf("an active goal: reason %q, context %q", reason, context)
	}
	vars["CODEX_SQLITE_HOME"] = goalGateTestGoalsDB(t, filepath.Join(dir, "complete"),
		"CREATE TABLE thread_goals (thread_id TEXT PRIMARY KEY NOT NULL, status TEXT NOT NULL, objective TEXT)",
		"INSERT INTO thread_goals VALUES ('s1', 'complete', 'ship')")
	if out := guard("request_user_input"); out != "" {
		t.Errorf("a completed goal is denied: %q", out)
	}
	vars["CODEX_SQLITE_HOME"] = goalGateTestGoalsDB(t, filepath.Join(dir, "no-table"), "CREATE TABLE unrelated (x INTEGER)")
	if _, context := goalGateTestDeny(t, guard("request_user_input")); context != goalGateModeDenyReason+" (goal-active=unreadable)" {
		t.Errorf("an unreadable goal: %q", context)
	}
	// Another tool never reads the database: the path below holds a file that is not one, which would answer
	// unreadable if it were read.
	vars["CODEX_SQLITE_HOME"] = filepath.Join(dir, "garbage")
	if err := os.MkdirAll(vars["CODEX_SQLITE_HOME"], 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(vars["CODEX_SQLITE_HOME"], host.GoalsDBFilename), []byte("not a database"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, tool := range []string{"shell", "request_user_input_async", "send_user_message_async"} {
		if out := guard(tool); out != "" {
			t.Errorf("%s read the goal database: %q", tool, out)
		}
	}
}

// goal-gate.test.ts:191-196 — the loose detector.
func TestGoalGateRawLooksLikeRequestUserInput(t *testing.T) {
	for raw, want := range map[string]bool{
		goalGateTestPayload(t, "/tmp/x", "s1", "request_user_input", map[string]any{}): true,
		goalGateTestPayload(t, "/tmp/x", "s1", "shell", map[string]any{}):              false,
		"not json": false,
		"":         false,
		// The loose detector needs neither session_id nor cwd, so it sees a payload the strict parse rejects.
		"{\"hook_event_name\":\"PreToolUse\",\"tool_name\":\"request_user_input\"}": true,
	} {
		if got := goalGateRawLooksLikeRequestUserInput(raw); got != want {
			t.Errorf("%q: %v", raw, got)
		}
	}
}

// goal-gate.test.ts:186-263 — the dispatcher: its order, its pass-through of a payload that does not parse, and
// the fail-closed recover for a request_user_input call whose goal state cannot be read.
func TestGoalGateHandlePreToolUseFailClosed(t *testing.T) {
	vars, env := goalGateTestEnv(t)
	cwd := t.TempDir()
	vars["CODEX_SQLITE_HOME"] = goalGateTestGoalsDB(t, filepath.Join(t.TempDir(), "codex"),
		"CREATE TABLE thread_goals (thread_id TEXT PRIMARY KEY NOT NULL, status TEXT NOT NULL, objective TEXT)",
		"INSERT INTO thread_goals VALUES ('s1', 'active', 'ship')")
	interview := goalGateTestPayload(t, cwd, "s1", "request_user_input", map[string]any{"questions": []any{}})
	shell := goalGateTestPayload(t, cwd, "s1", "shell", map[string]any{"command": "ls"})
	deps := goalGateRealDeps(env)

	if _, context := goalGateTestDeny(t, goalGateHandle(interview, true, deps)); context != goalGateModeDenyReason+" (goal-active=active)" {
		t.Errorf("an active goal through the dispatcher: %q", context)
	}
	if out := goalGateHandle(shell, true, deps); out != "" {
		t.Errorf("another tool through the dispatcher: %q", out)
	}
	budgeted := goalGateTestPayload(t, cwd, "s1", "create_goal", map[string]any{"objective": "x", "token_budget": 5})
	if _, reason := goalGateTestDeny(t, goalGateHandle(budgeted, true, deps)); !strings.Contains(reason, "token_budget") {
		t.Errorf("the budget guard through the dispatcher: %q", reason)
	}
	// A payload that does not parse is not a throw: it passes through (goal-gate.ts:316-317).
	for _, raw := range []string{"not json", "", "[]"} {
		if out := goalGateHandle(raw, true, deps); out != "" {
			t.Errorf("%q: %q", raw, out)
		}
	}
	// With PABCD off the interview guard is skipped, and the budget guard still fires.
	if out := goalGateHandle(interview, false, deps); out != "" {
		t.Errorf("request_user_input with PABCD off: %q", out)
	}
	if _, reason := goalGateTestDeny(t, goalGateHandle(budgeted, false, deps)); !strings.Contains(reason, "token_budget") {
		t.Errorf("a budgeted create_goal with PABCD off: %q", reason)
	}

	// The fail-closed recover: the goal-state lookup cannot answer, so a request_user_input call is denied as
	// unreadable rather than allowed (goal-gate.ts:321-324).
	broken := deps
	broken.GoalStatus = func(string) host.GoalStatus { panic("the goals database cannot answer") }
	if _, context := goalGateTestDeny(t, goalGateHandle(interview, true, broken)); context != goalGateModeDenyReason+" (goal-active=unreadable)" {
		t.Errorf("a failing lookup on request_user_input: %q", context)
	}
	// The recover reads PABCD enabled itself: with it off, the catch passes the payload through.
	off := broken
	off.PabcdEnabled = func(string) bool { return false }
	if out := goalGateHandle(interview, true, off); out != "" {
		t.Errorf("a failing lookup with PABCD off: %q", out)
	}
	// A payload that is not request_user_input never reaches the lookup, so nothing panics and nothing is denied.
	if out := goalGateHandle(shell, true, broken); out != "" {
		t.Errorf("a failing lookup on another tool: %q", out)
	}
}

// The goal-complete row of this issue: the guard is registered and the dispatcher's place for it is wired. The
// body is goalgate_complete.go (CRW-752); its own cases live in goalgate_complete_test.go. Here the row answers
// nothing for a session with no state and for another tool, which is the oracle's answer for every update_goal
// the gate does not deny (goal-gate.test.ts:290-301).
func TestGoalGateCompleteGuardAnswersNothingYet(t *testing.T) {
	goalGateTestEnv(t)
	cwd := t.TempDir()
	for _, input := range []any{map[string]any{"status": "complete"}, map[string]any{"status": "blocked"}, nil} {
		p := goalGatePreToolUse{SessionID: "s1", Cwd: cwd, ToolName: goalGateUpdateGoalToolName, ToolInput: input}
		if out := goalGateApplyGoalCompleteGuard(p, true); out != "" {
			t.Errorf("%v: %q", input, out)
		}
	}
	if out := goalGateApplyGoalCompleteGuard(goalGatePreToolUse{ToolName: goalGateCreateGoalToolName}, true); out != "" {
		t.Errorf("another tool: %q", out)
	}
}
