package journal

import (
	"errors"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/flow/backend/agent"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/engine"
)

// CRW-1372 (1): a wake the relay already staged stages nothing; StageWake says so (engine.WakeNotStaged with the channel's reason), so
// the session_wake row says staged:false instead of staged:true. (3): the session host adapter's marker-search warnings go to the run's
// journal.

func TestStageWakeAnswersThatItStagedNothing(t *testing.T) {
	st, _ := newStore(t)
	j := newJournal(t, st)
	j.Append(engine.Event{RunID: runID, Kind: engine.KindRun, Event: "started", Data: map[string]any{"workflow": "w", "script_digest": "d", "args_digest": "a"}})
	if err := j.Flush(); err != nil {
		t.Fatal(err)
	}
	wake := StageWake(st)
	w := engine.SessionWake{Run: runID, Owner: identity.ActorTaskID, Batch: "b1", ID: "b1", Ops: []string{"c1/t1-a1"}}
	if err := wake(ctx, w); err != nil {
		t.Fatalf("the first wake: %v", err)
	}
	err := wake(ctx, w)
	var not engine.WakeNotStaged
	if !errors.As(err, &not) || !strings.Contains(not.Reason, "already staged") {
		t.Fatalf("the same wake again answered %v, want WakeNotStaged", err)
	}
	if err := wake(ctx, engine.SessionWake{Run: runID, Owner: identity.ActorTaskID, Batch: "b1", ID: "b1/e2", Ops: []string{"c1/t1-a1"}}); err != nil {
		t.Fatalf("the wake of the next process: %v", err)
	}
}

func TestSessionHostWarningsGoToTheRunJournal(t *testing.T) {
	st, _ := newStore(t)
	j := newJournal(t, st)
	alloc, err := agent.NewAllocator(agent.AllocatorConfig{Repo: t.TempDir(), StateDir: t.TempDir(), RunID: runID})
	if err != nil {
		t.Fatal(err)
	}
	_, cfg, err := SessionEnv(SessionEnvRequest{Store: st, Allocator: alloc, StateDir: t.TempDir(), RunID: runID, Owner: identity.ActorTaskID, OwnerCwd: t.TempDir(),
		BoundDir: t.TempDir(), Journal: j})
	if err != nil {
		t.Fatal(err)
	}
	host, ok := cfg.Host.(agent.SessionHost)
	if !ok || host.Warn == nil {
		t.Fatalf("the session host %T has no Warn", cfg.Host)
	}
	host.Warn("c1/t1-a1", []string{"no_ledger_line: the hook's ledger has no line for the op"})
	var found bool
	for _, ev := range j.Events() {
		if ev.Kind == engine.KindFlow && ev.Event == "session_host_warning" && ev.Data["op"] == "c1/t1-a1" {
			found = true
		}
	}
	if !found {
		t.Fatalf("no session_host_warning row: %+v", j.Events())
	}
}
