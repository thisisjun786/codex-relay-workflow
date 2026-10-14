package cli

import (
	"context"
	"errors"
	"maps"
	"os"
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

// CRW-1372 (2), Argon V5 F1 of 4d293b7b and G1/G2 of fbed54e9: `crw flow next` that resumes a run with no process tells the story of
// the process it resumed. That process reads the role store as it is now, pins it in its own started row (journaling policy_repinned
// when it moved since the earlier process pinned) and binds the owner's line with it right after: next prints those pairs' spawn
// formats once that row and that line are there, writes no line, and records no role_store_changed row. The processes here stand in
// for the real one in the order it keeps (session_next_invariant_test.go runs the real one): process lock and run lock, then the
// started row naming the process lock, then the line.

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

// standInProcess is a process of the run in the order the run process keeps (CRW-1372): it holds its process lock and the run lock at
// once, and after delay records its started row (naming its process lock, pinning table), then the extra rows (a policy_repinned row),
// then binds the owner's line with table unless unbound. Its locks are held until the test ends; done closes once it bound.
func standInProcess(t *testing.T, w *world, run string, table *engine.Policy, delay time.Duration, unbound bool, extra ...engine.Event) <-chan struct{} {
	t.Helper()
	process, err := takeProcessLock(w.state, run)
	if err != nil {
		t.Fatal(err)
	}
	lock, err := TakeLock(w.state, run, "_run", time.Now())
	if err != nil {
		process.Release()
		t.Fatal(err)
	}
	t.Cleanup(func() { lock.Release(); process.Release() })
	rows := map[string]any{}
	for role, spec := range table.Roles {
		rows[role] = map[string]any{"model": spec.Pair.Model, "effort": spec.Pair.Effort}
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		time.Sleep(delay)
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
			"policy_digest": table.Digest, "role_pairs": rows, "process": process.Token()}})
		for _, ev := range extra {
			j.Append(ev)
		}
		if err := j.Close(); err != nil {
			t.Error(err)
			return
		}
		if unbound {
			return
		}
		if res, err := bindOwner(w.state, run, sessOwner, table); err != nil || !(res.Bound || res.Already) {
			t.Errorf("the process's bind: %+v %v", res, err)
		}
	}()
	return done
}

// ownerLines counts the lines of the owner thread's flow-bound file.
func ownerLines(t *testing.T, thread string) int {
	t.Helper()
	dir, err := bound.Dir()
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, thread+".jsonl"))
	if errors.Is(err, os.ErrNotExist) {
		return 0
	}
	if err != nil {
		t.Fatal(err)
	}
	return strings.Count(string(raw), "\n")
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

// The resumed process holds the run when next looks, and records its started row and binds its line a moment later: next answers with
// that row's pairs and that line, which it did not write.
func TestNextThatResumesARunTakesThePairsTheResumedProcessPinned(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	fastNextPoll(t)
	w := newWorld(t)
	td := w.deps()
	run := "run-0123456789b4"
	deadPinnedRun(t, w, run)
	resumed := map[string]engine.Pair{}
	for _, role := range bound.OrchestratorRoles {
		resumed[role] = engine.Pair{Model: "resumed-" + role, Effort: "medium"}
	}
	table := withPinnedPairs(engine.FakePolicy(), resumed, "sha256:resumed")
	spawn := td.Spawn
	var processBound <-chan struct{}
	td.Spawn = func(ctx context.Context, req SpawnRequest) (int, error) {
		// `crw flow _run --resume`: it holds the run, then pins the table it runs on in its started row and binds the owner with it
		processBound = standInProcess(t, w, run, table, 100*time.Millisecond, false)
		return spawn(ctx, req)
	}
	code, out, errOut := td.do("next", run, "--json", "--state", w.state)
	got := bound.Lookup(sessOwner).Line
	if code != 0 {
		t.Fatalf("next: %d %s %s", code, out, errOut)
	}
	<-processBound
	ans := decodeNext(t, out)
	if ans.Resumed == 0 {
		t.Fatalf("next did not resume the run: %+v", ans)
	}
	if got.RunID != run || got.PolicyDigest != "sha256:resumed" || got.HelperPairs["executor"].Model != "resumed-executor" {
		t.Fatalf("the owner's line when next answered is %+v, want the resumed process's (sha256:resumed)", got)
	}
	if n := ownerLines(t, sessOwner); n != 1 {
		t.Fatalf("%d lines in the owner's file: next wrote one of its own", n)
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

// The resumed process holds the run and has recorded nothing yet when next looks: next reads again until it has pinned the role store as
// it reads it (what the resume reads) and bound the owner with it, and it warns from that process's policy_repinned row that the resume
// takes it over what the earlier process pinned, recording nothing of its own.
func TestNextThatResumesARunTakesTheRoleStoreTheResumeReads(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	fastNextPoll(t)
	w := newWorld(t)
	td := w.deps()
	run := "run-0123456789b5"
	deadPinnedRun(t, w, run)
	storeTable, _, err := engine.LoadPolicy()
	if err != nil {
		t.Fatal(err)
	}
	spawn := td.Spawn
	td.Spawn = func(ctx context.Context, req SpawnRequest) (int, error) {
		standInProcess(t, w, run, storeTable, 150*time.Millisecond, false, engine.Event{RunID: run, Kind: engine.KindRun, Event: engine.EvPolicyRepinned,
			Data: map[string]any{"old": "sha256:pinned", "new": storeTable.Digest}})
		return spawn(ctx, req)
	}
	code, out, errOut := td.do("next", run, "--json", "--state", w.state)
	got := bound.Lookup(sessOwner).Line
	if code != 0 {
		t.Fatalf("next: %d %s %s", code, out, errOut)
	}
	ans := decodeNext(t, out)
	if ans.Resumed == 0 {
		t.Fatalf("next did not resume the run: %+v", ans)
	}
	if got.RunID != run || got.PolicyDigest != storeTable.Digest || !maps.Equal(got.HelperPairs, ownerPairs(storeTable)) {
		t.Fatalf("the owner's line when next answered is %+v, want the role store's %q %+v", got, storeTable.Digest, ownerPairs(storeTable))
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

// next writes no line of the run's (CRW-1372): a line of the run with other pairs than its process pinned stands, and a line the process
// did not write (its bind failed) is read again until startingWait and then reported, not written.
func TestNextAfterAResumeLeavesTheRunsOwnLine(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	fastNextPoll(t)
	saved := startingWait
	startingWait = 300 * time.Millisecond
	t.Cleanup(func() { startingWait = saved })
	w := newWorld(t)
	td := w.deps()
	run := "run-0123456789b6"
	w.newRun(run, sessOwner).close()
	sessionSpec(t, w, run)
	other := withPinnedPairs(engine.FakePolicy(), map[string]engine.Pair{"executor": {Model: "resumed-executor", Effort: "low"}}, "sha256:resumed")
	if res, err := bindOwner(w.state, run, sessOwner, other); err != nil || !res.Bound {
		t.Fatalf("bind: %+v %v", res, err)
	}
	// the process holds the run and pinned the role store's table, and its bind did not write
	<-standInProcess(t, w, run, engine.FakePolicy(), 0, true)
	code, out, errOut := td.do("next", run, "--json", "--state", w.state)
	if code != 0 {
		t.Fatalf("next: %d %s %s", code, out, errOut)
	}
	ans := decodeNext(t, out)
	if got := bound.Lookup(sessOwner).Line; got.PolicyDigest != "sha256:resumed" || ownerLines(t, sessOwner) != 1 {
		t.Fatalf("the owner's line %+v (%d lines): next wrote over the run's line", got, ownerLines(t, sessOwner))
	}
	reported := false
	for _, warning := range ans.Warnings {
		reported = reported || (strings.Contains(warning, "sha256:resumed") && strings.Contains(warning, "next writes no line"))
	}
	if !reported || ans.State == NextStarting {
		t.Fatalf("next did not report the line it found (%s): %v", ans.State, ans.Warnings)
	}
	// a thread with no line of the run: next reports it and writes none
	if _, err := closeOwner(run, sessOwner); err != nil {
		t.Fatal(err)
	}
	_, out, _ = td.do("next", run, "--json", "--state", w.state)
	missing := decodeNext(t, out)
	if got := bound.Lookup(sessOwner); got.State == bound.Orchestrator || ownerLines(t, sessOwner) != 2 {
		t.Fatalf("the owner's binding %+v (%d lines): next wrote a line", got, ownerLines(t, sessOwner))
	}
	if len(missing.Warnings) != 1 || !strings.Contains(missing.Warnings[0], "holds no open line of run "+run) {
		t.Fatalf("next did not report the missing line: %v", missing.Warnings)
	}
}

// A paused run next does not resume is resumed by another command while next waits: that process holds the run, then records its
// started row, then binds; next reads the run in between and takes the pairs of that process's own row, never the ones the ended
// process had pinned, and records nothing.
func TestNextTakesThePairsOfAProcessThatTookTheRunWhileItWaited(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
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
	fastNextPoll(t)
	resumed := map[string]engine.Pair{}
	for _, role := range bound.OrchestratorRoles {
		resumed[role] = engine.Pair{Model: "resumed-" + role, Effort: "medium"}
	}
	table := withPinnedPairs(engine.FakePolicy(), resumed, "sha256:resumed")
	done := make(chan struct{})
	go func() {
		defer close(done)
		time.Sleep(200 * time.Millisecond)
		// `crw flow resume RUN` from another command: its process holds the run, and 300ms later pins its table and binds the owner
		<-standInProcess(t, w, run, table, 300*time.Millisecond, false)
	}()
	code, out, errOut := td.do("next", run, "--wait", "10", "--json", "--state", w.state)
	got := bound.Lookup(sessOwner).Line
	<-done
	if code != 0 {
		t.Fatalf("next: %d %s %s", code, out, errOut)
	}
	ans := decodeNext(t, out)
	if ans.Resumed != 0 || len(td.spawned) != 0 {
		t.Fatalf("next resumed a paused run: %+v", ans)
	}
	if got.RunID != run || got.PolicyDigest != "sha256:resumed" {
		t.Fatalf("the owner's line when next answered is %+v, want the other resume's (sha256:resumed)", got)
	}
	noPinnedSpawns(t, ans, func(role string) string { return "resumed-" + role })
	if n := roleStoreChangedRows(t, w, run); n != 0 {
		t.Fatalf("%d role_store_changed rows: next recorded the ended process's pins as the live run's", n)
	}
}
