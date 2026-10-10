package hook

import (
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/host"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/state"
)

// CRW-1133. A synthetic host plays codex's goal lifecycle over a goals database: create_goal stores the
// token_budget the call carried, usage accrues, the host moves the goal to budget_limited when the budget is
// spent, and a resume is refused until the user has raised the limit. CRW never writes that database, so what
// this test pins is that the create_goal leg lets the user's limit reach the host and that, from then on, the
// hooks only read the goal: the limit the host holds is the one the user named, in every state.
type syntheticHost struct {
	t  *testing.T
	db *sql.DB
}

func newSyntheticHost(t *testing.T, dir, thread string) *syntheticHost {
	t.Helper()
	home := sessionHookGoalsDB(t, dir,
		"CREATE TABLE thread_goals (thread_id TEXT PRIMARY KEY NOT NULL, status TEXT NOT NULL, objective TEXT, token_budget INTEGER, tokens_used INTEGER NOT NULL DEFAULT 0)")
	db, err := sql.Open("sqlite", filepath.Join(home, host.GoalsDBFilename))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return &syntheticHost{t: t, db: db}
}

func (h *syntheticHost) exec(query string, args ...any) {
	h.t.Helper()
	if _, err := h.db.Exec(query, args...); err != nil {
		h.t.Fatal(err)
	}
}

// create is the host's create_goal: a token_budget in the call is the goal's limit.
func (h *syntheticHost) create(thread string, budget *int64) {
	h.t.Helper()
	h.exec("INSERT INTO thread_goals (thread_id, status, objective, token_budget) VALUES (?, 'active', 'ship the feature', ?)", thread, budget)
}

// spend is the host's accounting: the goal is budget_limited once the limit is reached.
func (h *syntheticHost) spend(thread string, tokens int64) {
	h.t.Helper()
	h.exec("UPDATE thread_goals SET tokens_used = tokens_used + ? WHERE thread_id = ?", tokens, thread)
	h.exec("UPDATE thread_goals SET status = 'budget_limited' WHERE thread_id = ? AND status = 'active' AND token_budget IS NOT NULL AND tokens_used >= token_budget", thread)
}

// resume is the host's resume of a budget_limited goal: it stays limited while the budget is spent.
func (h *syntheticHost) resume(thread string) {
	h.t.Helper()
	h.exec("UPDATE thread_goals SET status = 'active' WHERE thread_id = ? AND status = 'budget_limited' AND (token_budget IS NULL OR tokens_used < token_budget)", thread)
}

// raise is the user's own decision to give the goal a larger limit.
func (h *syntheticHost) raise(thread string, budget int64) {
	h.t.Helper()
	h.exec("UPDATE thread_goals SET token_budget = ? WHERE thread_id = ?", budget, thread)
}

func (h *syntheticHost) row(thread string) (status string, budget sql.NullInt64, used int64) {
	h.t.Helper()
	if err := h.db.QueryRow("SELECT status, token_budget, tokens_used FROM thread_goals WHERE thread_id = ?", thread).Scan(&status, &budget, &used); err != nil {
		h.t.Fatal(err)
	}
	return status, budget, used
}

func TestSyntheticHostGoalLifecycleKeepsTheUsersBudget(t *testing.T) {
	cwd, env := stopRig(t, "")
	dir, _ := env("HOME")
	codex := filepath.Join(filepath.Dir(dir), "codex-budget")
	sim := newSyntheticHost(t, codex, stopSID)
	vars := map[string]string{"CRW_BIN": "CRW", "HOME": dir, "CODEX_SQLITE_HOME": codex}
	env = sessionHookEnv(vars)
	deps := goalGateRealDeps(env)

	// The user asked for 1000 tokens: the call carries it, the leg lets it through, the host stores it.
	const limit int64 = 1000
	create := goalGateTestPayload(t, cwd, stopSID, "create_goal", map[string]any{"objective": "ship the feature", "token_budget": limit})
	if out := goalGateHandle(create, true, deps); out != "" {
		t.Fatalf("the create_goal leg refused the user's budget: %q", out)
	}
	budget := limit
	sim.create(stopSID, &budget)

	ask := goalGateTestPayload(t, cwd, stopSID, "request_user_input", map[string]any{"questions": []any{}})
	stopInFlight(t, cwd, state.PhaseB)
	blocks := func() bool { return stopRun(cwd, env) != (StopAnswer{}) }
	interviewDenied := func() bool { return goalGateHandle(ask, true, deps) != "" }
	unchanged := func(wantStatus string, wantBudget, wantUsed int64) {
		t.Helper()
		status, got, used := sim.row(stopSID)
		if status != wantStatus || !got.Valid || got.Int64 != wantBudget || used != wantUsed {
			t.Fatalf("the goal row is %q budget %v used %d, want %q budget %d used %d: a hook changed what the host holds", status, got, used, wantStatus, wantBudget, wantUsed)
		}
	}

	// Active, under the limit: the goal owns the thread (Stop blocks, the Interview is denied).
	sim.spend(stopSID, 400)
	if !blocks() || !interviewDenied() {
		t.Fatal("an active goal under its limit must own the thread")
	}
	unchanged("active", limit, 400)

	// The host spends the budget: budgetLimited. The hooks read it as a goal that is not active and leave the limit alone.
	sim.spend(stopSID, 700)
	if blocks() || interviewDenied() {
		t.Fatal("a budget_limited goal must release Stop and the Interview")
	}
	unchanged("budget_limited", limit, 1100)

	// Resume is the host's: it holds the goal at the limit the user named while that limit is spent.
	sim.resume(stopSID)
	if blocks() || interviewDenied() {
		t.Fatal("a resume with the budget spent must leave the goal budget_limited")
	}
	unchanged("budget_limited", limit, 1100)

	// The user raises the limit; the resume then reactivates the goal and the hooks follow, still on the user's number.
	sim.raise(stopSID, 5000)
	sim.resume(stopSID)
	if !blocks() || !interviewDenied() {
		t.Fatal("a resumed goal must own the thread again")
	}
	unchanged("active", 5000, 1100)
}
