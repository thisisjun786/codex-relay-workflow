package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/appserver"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/backend/agent"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/engine"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/journal"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/dag"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// OpsFor installs the operations of the project lane (`crw.*`) for a run: they are built on the relay's scheduler, which the
// binary that has the adapter installs here. A build that installs none runs the workflows that call no `crw.*` operation, and a
// workflow that calls one ends in a script error that names it.
var OpsFor func(ctx context.Context, req WorldRequest, j engine.Journal) (map[string]engine.Op, engine.Prover, func(ctx context.Context, req engine.SnapshotRequest) (string, error), error)

// ProductionWorld is the World of the shipped binary: the relay store the run's journal is persisted in, the App Server
// connection of its host, and the allocator of its working directories (<state>/flow/<run>/...). The backend binds every engine
// turn to the run before it starts it, as a resumed run does (agent.BoundConfig), and the run hands what goes up from it to the
// hook journal.UpwardFactory makes, when the binary installed one. The store is opened for writing; a store that does not exist
// is an error, never a new empty store.
func ProductionWorld(ctx context.Context, req WorldRequest) (World, error) {
	spec := req.Spec
	socket := spec.Socket
	if socket == "" {
		var err error
		if socket, err = store.DefaultSocket(); err != nil {
			return World{}, err
		}
	}
	db, statePath := filepath.Join(req.State, "relay.sqlite3"), req.State
	if sel, err := store.ResolveStateDir(req.State, socket); err == nil {
		db, statePath = sel.DBPath(), sel.Path
	}
	if _, err := os.Stat(db); err != nil {
		return World{}, fmt.Errorf("there is no relay store at %s: %w", db, err)
	}
	repo, err := repository(ctx, spec.Repo)
	if err != nil {
		return World{}, err
	}
	table, _, err := engine.LoadPolicy()
	if err != nil {
		return World{}, err
	}
	st, err := store.Open(ctx, db, socket)
	if err != nil {
		return World{}, err
	}
	id := journal.Identity{}
	if !req.Resume {
		if id, err = RunIdentity(ctx, st, spec); err != nil {
			_ = st.Close()
			return World{}, err
		}
	}
	j, err := journal.Open(ctx, st, spec.RunID, id, journal.Options{})
	if err != nil {
		_ = st.Close()
		return World{}, err
	}
	alloc, err := agent.NewAllocator(agent.AllocatorConfig{Repo: repo, StateDir: req.State, RunID: spec.RunID})
	if err != nil {
		_ = st.Close()
		return World{}, err
	}
	threads := agent.NewThreads()
	client := appserver.New(socket, appserver.DefaultBounds, agent.ClientOption(threads))
	backend, err := agent.New(agent.Config{Client: client, Allocator: alloc, Threads: threads, Bound: &agent.BoundConfig{RunID: spec.RunID, Policy: table}})
	if err != nil {
		_ = client.Close()
		_ = st.Close()
		return World{}, err
	}
	w := World{Journal: j, Backend: backend, Policy: table}
	if journal.UpwardFactory != nil {
		// what goes up from the run (CRW-1204) is handed to the hook the binary installs, as the engine's resume command does
		// (journal.ResumeEnv): the relay is the one the run is in
		w.Upward = journal.UpwardFactory(st, j, spec.RunID, statePath, socket)
	}
	if OpsFor != nil {
		if w.Ops, w.Prover, w.InputSnapshot, err = OpsFor(ctx, req, j); err != nil {
			backend.Close()
			_ = client.Close()
			_ = st.Close()
			return World{}, err
		}
	}
	w.Close = func() error {
		backend.Close()
		return errors.Join(j.Close(), client.Close(), st.Close())
	}
	return w, nil
}

// RunIdentity is what the run record of a new run holds besides what the engine knows: the owner thread and the plan the run
// names with its arguments ("plan") at the revision the plan has now, which the run is pinned to for as long as it lives (the
// plan basis an answer is judged against). A run that names no plan pins none.
func RunIdentity(ctx context.Context, st *store.Store, spec Spec) (journal.Identity, error) {
	id := journal.Identity{ActorTaskID: spec.Thread, PlanID: planOf(spec.Args)}
	if id.PlanID == "" {
		return id, nil
	}
	_, head, err := (&dag.Repo{Store: st}).Snapshot(ctx, id.PlanID, 0)
	if err != nil {
		return journal.Identity{}, fmt.Errorf("the plan %s the run names cannot be read from the relay store: %w", id.PlanID, err)
	}
	id.PlanRevision = head
	return id, nil
}

// planOf is the plan a run's arguments name ("plan"), or "".
func planOf(args json.RawMessage) string {
	var a struct {
		Plan string `json:"plan"`
	}
	_ = json.Unmarshal(args, &a)
	return a.Plan
}

// repository is the git repository a run's worktrees are made from: the directory the operator named, else the top level of
// the repository the working directory is in.
func repository(ctx context.Context, named string) (string, error) {
	dir := named
	if dir == "" {
		dir = "."
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", err
	}
	out, err := exec.CommandContext(ctx, "git", "-C", abs, "rev-parse", "--show-toplevel").Output()
	if err != nil {
		return "", fmt.Errorf("%s is not inside a git repository (name one with --repo): %w", abs, err)
	}
	return strings.TrimSpace(string(out)), nil
}
