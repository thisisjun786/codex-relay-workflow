package cli

import (
	"strings"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/flow/bound"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/engine"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/report"
)

// CRW-1372 (2): `crw flow next` writes the owner's orchestrator line with the pairs the live run pinned (its newest started row), not
// with the role store read again: a role store changed while the run lives is warned about and recorded once, and the line stays the
// run's. (5): the hint names the open subagents wait_agent waits on.

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
	w := newWorld(t)
	td := w.deps()
	run := "run-0123456789b0"
	lock, want := pinnedRun(t, w, run)
	defer lock.Release()
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
	if len(ans.Spawns) != 6 || ans.Spawns[0].Model != "pinned-"+ans.Spawns[0].Role {
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
	if rows() != 1 {
		t.Fatalf("%d role_store_changed rows, want 1", rows())
	}
	// next again: the line is the run's (nothing rewritten, no 'missing' warning), and the change is recorded once
	_, out, _ = td.do("next", run, "--json", "--state", w.state)
	again := decodeNext(t, out)
	for _, warning := range again.Warnings {
		if strings.Contains(warning, "missing") {
			t.Fatalf("the second next rewrote the line: %v", again.Warnings)
		}
	}
	if rows() != 1 {
		t.Fatalf("%d role_store_changed rows after the second next", rows())
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
