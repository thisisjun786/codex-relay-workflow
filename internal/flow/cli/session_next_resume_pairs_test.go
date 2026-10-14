package cli

import (
	"context"
	"maps"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/flow/bound"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/engine"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/journal"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/report"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// CRW-1372 (2), Argon V5 F1 of 4d293b7b: `crw flow next` that resumes a run with no process tells the story of the process it resumed.
// That process reads the role store as it is now, binds the owner's line with it and pins it in its own started row (journaling
// policy_repinned when it moved since the earlier process pinned): next prints those pairs' spawn formats, leaves that process's line
// standing, and records no role_store_changed row of a store that "changed while the run lives" for the process that is gone.

// deadPinnedRun is pinnedRun whose process has ended: no lock, not paused, not over.
func deadPinnedRun(t *testing.T, w *world, run string) {
	t.Helper()
	lock, _ := pinnedRun(t, w, run)
	lock.Release()
}

func roleStoreChangedRows(t *testing.T, w *world, run string) int {
	t.Helper()
	rows, err := report.ReadRows(ctx, w.st.DB, run, KindRoleStoreChanged)
	if err != nil {
		t.Fatal(err)
	}
	return len(rows)
}

func noPinnedSpawns(t *testing.T, ans NextAnswer, model func(role string) string) {
	t.Helper()
	if len(ans.Spawns) != 6 {
		t.Fatalf("the spawn formats %+v", ans.Spawns)
	}
	for _, sp := range ans.Spawns {
		if sp.Model != model(sp.Role) {
			t.Fatalf("role %s spawns with %q, want the resumed process's %q: %+v", sp.Role, sp.Model, model(sp.Role), ans.Spawns)
		}
	}
}

// The resumed process has bound and pinned before next looks again: next follows its started row.
func TestNextThatResumesARunTakesThePairsTheResumedProcessPinned(t *testing.T) {
	w := newWorld(t)
	td := w.deps()
	run := "run-0123456789b4"
	deadPinnedRun(t, w, run)
	resumed := map[string]engine.Pair{}
	rows := map[string]any{}
	for _, role := range bound.OrchestratorRoles {
		resumed[role] = engine.Pair{Model: "resumed-" + role, Effort: "medium"}
		rows[role] = map[string]any{"model": "resumed-" + role, "effort": "medium"}
	}
	table := withPinnedPairs(engine.FakePolicy(), resumed, "sha256:resumed")
	spawn := td.Spawn
	td.Spawn = func(ctx context.Context, req SpawnRequest) (int, error) {
		// `crw flow _run --resume`: it binds the owner with the table it runs on, then its started row pins it
		if res, err := bindOwner(req.State, run, sessOwner, table); err != nil || !(res.Bound || res.Already) {
			t.Fatalf("the resumed process's bind: %+v %v", res, err)
		}
		st, err := store.Open(ctx, filepath.Join(w.state, "relay.sqlite3"), "")
		if err != nil {
			t.Fatal(err)
		}
		j, err := journal.Open(ctx, st, run, journal.Identity{}, journal.Options{Immediate: true})
		if err != nil {
			t.Fatal(err)
		}
		j.Append(engine.Event{RunID: run, Kind: engine.KindRun, Event: "started", Data: map[string]any{"workflow": "hello", "backend": engine.BackendSession,
			"policy_digest": "sha256:resumed", "role_pairs": rows}})
		if err := j.Close(); err != nil {
			t.Fatal(err)
		}
		_ = st.Close()
		return spawn(ctx, req)
	}
	code, out, errOut := td.do("next", run, "--json", "--state", w.state)
	if code != 0 {
		t.Fatalf("next: %d %s %s", code, out, errOut)
	}
	ans := decodeNext(t, out)
	if ans.Resumed == 0 {
		t.Fatalf("next did not resume the run: %+v", ans)
	}
	if got := bound.Lookup(sessOwner).Line; got.RunID != run || got.PolicyDigest != "sha256:resumed" || got.HelperPairs["executor"].Model != "resumed-executor" {
		t.Fatalf("the owner's line after next is %+v, want the resumed process's (sha256:resumed)", got)
	}
	noPinnedSpawns(t, ans, func(role string) string { return "resumed-" + role })
	if n := roleStoreChangedRows(t, w, run); n != 0 {
		t.Fatalf("%d role_store_changed rows: next recorded a change for the process that is gone", n)
	}
	for _, warning := range ans.Warnings {
		if strings.Contains(warning, "while run "+run+" lives") {
			t.Fatalf("next warned of a live run's role store: %v", ans.Warnings)
		}
	}
}

// The resumed process holds the run and has not bound yet: next takes the role store as it reads it (what the resume reads), warns
// that the resume takes it over what the earlier process pinned, and records nothing of its own.
func TestNextThatResumesARunTakesTheRoleStoreTheResumeReads(t *testing.T) {
	w := newWorld(t)
	td := w.deps()
	run := "run-0123456789b5"
	deadPinnedRun(t, w, run)
	storeTable, _, err := engine.LoadPolicy()
	if err != nil {
		t.Fatal(err)
	}
	code, out, errOut := td.do("next", run, "--json", "--state", w.state)
	if code != 0 {
		t.Fatalf("next: %d %s %s", code, out, errOut)
	}
	ans := decodeNext(t, out)
	if ans.Resumed == 0 {
		t.Fatalf("next did not resume the run: %+v", ans)
	}
	if got := bound.Lookup(sessOwner).Line; got.RunID != run || got.PolicyDigest != storeTable.Digest || !maps.Equal(got.HelperPairs, ownerPairs(storeTable)) {
		t.Fatalf("the owner's line after next is %+v, want the role store's %q %+v", got, storeTable.Digest, ownerPairs(storeTable))
	}
	noPinnedSpawns(t, ans, func(role string) string { return storeTable.Roles[role].Pair.Model })
	if n := roleStoreChangedRows(t, w, run); n != 0 {
		t.Fatalf("%d role_store_changed rows: next recorded a change for the process that is gone", n)
	}
	warned := false
	for _, warning := range ans.Warnings {
		warned = warned || (strings.Contains(warning, "resum") && strings.Contains(warning, "sha256:pinned") && strings.Contains(warning, "policy_repinned"))
	}
	if !warned {
		t.Fatalf("no warning that the resume takes the role store over the pinned pairs: %v", ans.Warnings)
	}
}

// The resumed process binds before next does: under the flow-bound lock the run's own line is the run process's to write, and next
// leaves a line of the run that is already there whatever its pairs (bound.OrchestratorBinding.KeepRunLine).
func TestNextAfterAResumeLeavesTheRunsOwnLine(t *testing.T) {
	w := newWorld(t)
	run := "run-0123456789b6"
	other := withPinnedPairs(engine.FakePolicy(), map[string]engine.Pair{"executor": {Model: "resumed-executor", Effort: "low"}}, "sha256:resumed")
	if res, err := bindOwner(w.state, run, sessOwner, other); err != nil || !(res.Bound || res.Already) {
		t.Fatalf("bind: %+v %v", res, err)
	}
	res, err := bindMissingOwner(w.state, run, sessOwner, engine.FakePolicy())
	if err != nil || res.Bound || !res.Already {
		t.Fatalf("next's bind over the resumed process's line: %+v %v", res, err)
	}
	if got := bound.Lookup(sessOwner).Line; got.PolicyDigest != "sha256:resumed" {
		t.Fatalf("the owner's line %+v, want the resumed process's", got)
	}
}

// A paused run next does not resume is resumed by another command while next waits: when next finds a process holding the run, it
// reads the pairs that process pinned then, not the ones the ended process had pinned when next began.
func TestNextTakesThePairsOfAProcessThatTookTheRunWhileItWaited(t *testing.T) {
	w := newWorld(t)
	td := w.deps()
	run := "run-0123456789b7"
	r := w.newRun(run, sessOwner)
	pinned := map[string]any{}
	for _, role := range bound.OrchestratorRoles {
		pinned[role] = map[string]any{"model": "pinned-" + role, "effort": "high"}
	}
	r.j.Append(engine.Event{RunID: run, Kind: engine.KindRun, Event: "started", Data: map[string]any{"workflow": "hello", "backend": engine.BackendSession,
		"policy_digest": "sha256:pinned", "role_pairs": pinned}})
	r.flow(engine.EvRunPaused, map[string]any{"by": "operator", "reason": "crw flow pause"})
	r.close()
	sessionSpec(t, w, run)
	saved := nextPoll
	nextPoll = 20 * time.Millisecond
	t.Cleanup(func() { nextPoll = saved })
	resumed := map[string]engine.Pair{}
	rows := map[string]any{}
	for _, role := range bound.OrchestratorRoles {
		resumed[role] = engine.Pair{Model: "resumed-" + role, Effort: "medium"}
		rows[role] = map[string]any{"model": "resumed-" + role, "effort": "medium"}
	}
	table := withPinnedPairs(engine.FakePolicy(), resumed, "sha256:resumed")
	var lock *Lock
	done := make(chan struct{})
	go func() {
		defer close(done)
		time.Sleep(200 * time.Millisecond)
		// `crw flow resume RUN` from another command: its process binds the owner with the table it runs on and pins it, and holds the
		// run (the lock last, so next finds the process with its started row)
		if res, err := bindOwner(w.state, run, sessOwner, table); err != nil || !(res.Bound || res.Already) {
			t.Errorf("the other resume's bind: %+v %v", res, err)
			return
		}
		st, err := store.Open(ctx, filepath.Join(w.state, "relay.sqlite3"), "")
		if err != nil {
			t.Error(err)
			return
		}
		defer st.Close()
		j, err := journal.Open(ctx, st, run, journal.Identity{}, journal.Options{Immediate: true})
		if err != nil {
			t.Error(err)
			return
		}
		j.Append(engine.Event{RunID: run, Kind: engine.KindRun, Event: "started", Data: map[string]any{"workflow": "hello", "backend": engine.BackendSession,
			"policy_digest": "sha256:resumed", "role_pairs": rows}})
		if err := j.Close(); err != nil {
			t.Error(err)
			return
		}
		if lock, err = TakeLock(w.state, run, "_run", time.Now()); err != nil {
			t.Error(err)
		}
	}()
	code, out, errOut := td.do("next", run, "--wait", "10", "--json", "--state", w.state)
	<-done
	defer lock.Release()
	if code != 0 {
		t.Fatalf("next: %d %s %s", code, out, errOut)
	}
	ans := decodeNext(t, out)
	if ans.Resumed != 0 || len(td.spawned) != 0 {
		t.Fatalf("next resumed a paused run: %+v", ans)
	}
	if got := bound.Lookup(sessOwner).Line; got.RunID != run || got.PolicyDigest != "sha256:resumed" {
		t.Fatalf("the owner's line after next is %+v, want the other resume's (sha256:resumed)", got)
	}
	noPinnedSpawns(t, ans, func(role string) string { return "resumed-" + role })
	changed, err := report.ReadRows(ctx, w.st.DB, run, KindRoleStoreChanged)
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range changed {
		if row.Str("pinned_digest") == "sha256:pinned" {
			t.Fatalf("next recorded the ended process's pins as the live run's: %+v", row.Payload)
		}
	}
}
