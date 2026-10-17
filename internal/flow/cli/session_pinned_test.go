package cli

import (
	"strings"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/flow/bound"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/engine"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/report"
)

// CRW-1372 (2): `crw flow next` answers with the pairs the live run pinned (its newest started row), not with the role store read
// again, and writes no line: the owner's orchestrator line is the run process's, and a role store changed while the run lives is
// warned about and recorded by nobody (the run's next process records its pairs and the change). (5): the hint names the open
// subagents wait_agent waits on.

// pinnedRun is a live session run whose newest started row pinned pairs no role store holds.
func pinnedRun(t *testing.T, w *world, run string) (*Lock, map[string]bound.Pair) {
	t.Helper()
	r := w.newRun(run, sessOwner)
	pinned, want := map[string]any{}, map[string]bound.Pair{}
	for _, role := range bound.OrchestratorRoles {
		pinned[role] = map[string]any{"model": "pinned-" + role, "effort": "high"}
		want[role] = bound.Pair{Model: "pinned-" + role, Effort: "high"}
	}
	r.j.Append(engine.Event{RunID: run, Kind: engine.KindRun, Event: "started", Data: map[string]any{"workflow": "hello", "backend": engine.BackendSession,
		"policy_digest": "sha256:pinned", "role_pairs": pinned}})
	r.close()
	sessionSpec(t, w, run)
	lock, err := TakeLock(w.state, run, "_run", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	return lock, want
}

func TestNextBindsThePairsTheLiveRunPinned(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	w := newWorld(t)
	td := w.deps()
	run := "run-0123456789b0"
	lock, want := pinnedRun(t, w, run)
	defer lock.Release()
	// the run's process (one of a crw before the process lock: its started row names none) bound the owner with the pairs it pinned
	pinned := map[string]engine.Pair{}
	for role, pair := range want {
		pinned[role] = engine.Pair{Model: pair.Model, Effort: pair.Effort}
	}
	if res, err := bindOwner(w.state, run, sessOwner, withPinnedPairs(engine.FakePolicy(), pinned, "sha256:pinned")); err != nil || !res.Bound {
		t.Fatalf("the process's bind: %+v %v", res, err)
	}
	code, out, errOut := td.do("next", run, "--json", "--state", w.state)
	if code != 0 {
		t.Fatalf("next: %d %s %s", code, out, errOut)
	}
	ans := decodeNext(t, out)
	got := bound.Lookup(sessOwner)
	if got.State != bound.Orchestrator || got.Line.RunID != run || got.Line.PolicyDigest != "sha256:pinned" {
		t.Fatalf("the owner's binding %+v", got)
	}
	for role, pair := range want {
		if got.Line.HelperPairs[role] != pair {
			t.Fatalf("role %s is bound to %+v, want the pinned %+v", role, got.Line.HelperPairs[role], pair)
		}
	}
	if len(ans.Spawns) != len(bound.OrchestratorRoles) || ans.Spawns[0].Model != "pinned-"+ans.Spawns[0].Role {
		t.Fatalf("the spawn formats %+v are not the run's", ans.Spawns)
	}
	changed := false
	for _, warning := range ans.Warnings {
		changed = changed || strings.Contains(warning, "role store")
	}
	if !changed {
		t.Fatalf("no warning that the role store moved: %v", ans.Warnings)
	}
	rows := func() int {
		n, err := report.ReadRows(ctx, w.st.DB, run, "role_store_changed")
		if err != nil {
			t.Fatal(err)
		}
		return len(n)
	}
	if rows() != 0 {
		t.Fatalf("%d role_store_changed rows: next recorded a row of the run", rows())
	}
	// next again: the line is the run's process's (one line, nothing rewritten, nothing reported of it), and nothing is recorded
	_, out, _ = td.do("next", run, "--json", "--state", w.state)
	again := decodeNext(t, out)
	for _, warning := range again.Warnings {
		if strings.Contains(warning, "next writes no line") {
			t.Fatalf("the second next found the line not the process's: %v", again.Warnings)
		}
	}
	if n := ownerLines(t, sessOwner); n != 1 {
		t.Fatalf("%d lines in the owner's file: next wrote one", n)
	}
	if rows() != 0 {
		t.Fatalf("%d role_store_changed rows after the second next", rows())
	}
}

// TestCRW1589_PinnedPairsKeepUnitCheckOutOfTheAgentRoles (CRW-1589): a started row's role_pairs may name unit-check,
// the pair the row pins for the unit check tool; withPinnedPairs moves it to the table's tool pair and never makes it a
// spawnable role, and a row of a crw before it (no unit-check) keeps the table's own.
func TestCRW1589_PinnedPairsKeepUnitCheckOutOfTheAgentRoles(t *testing.T) {
	pinned := map[string]engine.Pair{
		"reviewer":   {Model: "pinned-reviewer", Effort: "high"},
		"unit-check": {Model: "pinned-check", Effort: "low"},
	}
	out := withPinnedPairs(engine.FakePolicy(), pinned, "sha256:pinned")
	if _, ok := out.Roles["unit-check"]; ok {
		t.Fatalf("unit-check became a table role: %v", out.Roles)
	}
	if out.UnitCheck != (engine.Pair{Model: "pinned-check", Effort: "low"}) {
		t.Fatalf("the pinned unit-check pair %+v", out.UnitCheck)
	}
	if out.Digest != "sha256:pinned" || out.Roles["reviewer"].Pair != (engine.Pair{Model: "pinned-reviewer", Effort: "high"}) {
		t.Fatalf("the pinned table %+v %s", out.Roles["reviewer"], out.Digest)
	}
	// A row written before unit-check was pinned keeps the table's own pair.
	old := withPinnedPairs(engine.FakePolicy(), map[string]engine.Pair{"reviewer": {Model: "pinned-reviewer", Effort: "high"}}, "sha256:old")
	if _, ok := old.Roles["unit-check"]; ok || old.UnitCheck != engine.FakePolicy().UnitCheck {
		t.Fatalf("an old row's table %+v %v", old.Roles, old.UnitCheck)
	}
}

func TestNextHintNamesTheOpenSubagents(t *testing.T) {
	w := newWorld(t)
	td := w.deps()
	run := "run-0123456789b1"
	r := w.newRun(run, sessOwner)
	r.call("c_000000000001", "explorer", "A", 1, []string{engine.EvPrepared, engine.EvThreadStarted},
		map[string]map[string]any{engine.EvThreadStarted: {"via": "session", "thread_id": "agent-9"}})
	r.close()
	sessionSpec(t, w, run)
	lock, err := TakeLock(w.state, run, "_run", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Release()
	_, out, _ := td.do("next", run, "--json", "--state", w.state)
	ans := decodeNext(t, out)
	if ans.Slots.Open != 1 || len(ans.Slots.Agents) != 1 || ans.Slots.Agents[0] != "agent-9" || !strings.Contains(ans.Hint, `wait_agent(targets=["agent-9"]`) {
		t.Fatalf("slots %+v hint %s", ans.Slots, ans.Hint)
	}
}
