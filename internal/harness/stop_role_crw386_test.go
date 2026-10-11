package harness

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/goalplan"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/state"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport/storeseed"
)

// seedStopRoles points the production role reader at a registry that gives session s1 the listed roles (none: an empty
// registry; absent: no registry at all).
func seedStopRoles(t *testing.T, absent bool, roles ...string) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "relay-state")
	t.Setenv("CODEX_SESSION_RELAY_STATE", dir)
	if absent {
		return
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(dir, "relay.sqlite3"), "")
	if err != nil {
		t.Fatal(err)
	}
	scopes := map[string][2]string{"parent": {"project", "P-1"}, "child": {"issue", "CRW-1"}, "supervisor": {"initiative", "I-1"}}
	for i, role := range roles {
		scope := scopes[role]
		row := store.ScopeBindingsRow{BindingID: "b" + string(rune('1'+i)), Role: role, ScopeKind: scope[0], ScopeKey: scope[1] + string(rune('a'+i)), TaskID: "s1", HostID: "h",
			Status: "active", Revision: 1, CreatedAt: "2026-10-11T00:00:00.000000+00:00", UpdatedAt: "2026-10-11T00:00:00.000000+00:00"}
		if err := storeseed.InsertScopeBinding(ctx, st, row); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
}

// stopIdleSetup is stopSetup with the session idle (no cycle in flight) and bound to a goalplan that has remaining work: the
// Stop that answers the goal-idle block.
func stopIdleSetup(t *testing.T) (cwd string, env map[string]string) {
	t.Helper()
	cwd, env = stopSetup(t, "active")
	s := state.DefaultState("s1", "")
	s.Slug = "export"
	if err := state.WriteState(cwd, s); err != nil {
		t.Fatal(err)
	}
	plan := goalplan.BuildGoalplan(goalplan.NewGoalplanInput{Objective: "export", Now: func() string { return "2026-01-01T00:00:00.000Z" }})
	plan.Slug = "export"
	plan.WorkPhases = []goalplan.GoalplanWorkPhase{{ID: "wp1", Title: "Exporter", Status: goalplan.WorkPhasePending, Tasks: []goalplan.GoalplanTask{}, CriteriaIDs: []string{}}}
	if err := goalplan.WriteGoalplan(cwd, plan); err != nil {
		t.Fatal(err)
	}
	return cwd, env
}

// CRW-386: the wired Stop leg against a seeded registry. A project parent and a supervisor are released with no output and no
// state change, even with a bound goalplan and remaining work; a dispatched task, a session with both roles, an unbound session
// and an absent registry (no role evidence) get the goal-idle block, with the same text, and spend the budget.
func TestStopLegGoalIdleFollowsTheRegistryRole(t *testing.T) {
	cases := []struct {
		name   string
		absent bool
		roles  []string
		skip   bool
	}{
		{"parent", false, []string{"parent"}, true},
		{"supervisor", false, []string{"supervisor"}, true},
		{"dispatched task", false, []string{"child"}, false},
		{"parent and child", false, []string{"parent", "child"}, false},
		{"parent and supervisor", false, []string{"parent", "supervisor"}, false},
		{"no binding", false, nil, false},
		{"no registry", true, nil, false},
	}
	var oracle string
	for _, c := range cases {
		if c.skip {
			continue
		}
		cwd, env := stopIdleSetup(t)
		seedStopRoles(t, c.absent, c.roles...)
		code, out, errOut := stopHook(Legs(), payload("Stop", cwd, `,"turn_id":"t1","stop_hook_active":false,"last_assistant_message":"Done.","transcript_path":null`), env)
		if code != 0 || errOut != "" || !strings.HasPrefix(out, `{"decision":"block","reason":"[crw — goal continuation] A host goal is ACTIVE but no PABCD cycle is in flight.`) {
			t.Fatalf("%s: %d %q %q", c.name, code, out, errOut)
		}
		if oracle == "" {
			oracle = out
		}
		if out != oracle {
			t.Errorf("%s: the block differs from the oracle's:\n got %q\nwant %q", c.name, out, oracle)
		}
		if s := state.ReadState(cwd, "s1"); s.StopBlockCount != 1 || s.StopBlockTotal != 1 {
			t.Errorf("%s: the block spends the budget once: %+v", c.name, s)
		}
	}
	for _, c := range cases {
		if !c.skip {
			continue
		}
		cwd, env := stopIdleSetup(t)
		seedStopRoles(t, c.absent, c.roles...)
		before, err := os.ReadFile(state.StatePath(cwd, "s1"))
		if err != nil {
			t.Fatal(err)
		}
		code, out, errOut := stopHook(Legs(), payload("Stop", cwd, `,"turn_id":"t1","stop_hook_active":false,"last_assistant_message":"Done.","transcript_path":null`), env)
		if code != 0 || out != "" || errOut != "" {
			t.Errorf("%s: want a silent release, got %d %q %q", c.name, code, out, errOut)
		}
		if after, _ := os.ReadFile(state.StatePath(cwd, "s1")); string(after) != string(before) {
			t.Errorf("%s: the release changed the state", c.name)
		}
	}
}

// CRW-386: only the goal-idle block is withheld from a parent; the in-flight continuation (a cycle the parent itself started)
// is the oracle's, whatever the registry says.
func TestStopLegInFlightBlockIsNotRoleDependent(t *testing.T) {
	cwd, env := stopSetup(t, "active")
	seedStopRoles(t, false, "parent")
	_, out, _ := stopHook(Legs(), payload("Stop", cwd, `,"turn_id":"t1"`), env)
	if !strings.HasPrefix(out, `{"decision":"block","reason":"[crw — continue PABCD] You are mid-cycle at B`) {
		t.Errorf("a parent's in-flight Stop: %q", out)
	}
}
