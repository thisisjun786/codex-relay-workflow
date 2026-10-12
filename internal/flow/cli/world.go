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
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/audit"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/backend/agent"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/engine"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/journal"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/dag"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// OpsFor installs the operations of the project lane (`crw.*`) for a run: they are built on the relay's scheduler, which the binary
// installs here (cmd/crw/main.go assigns SchedulerLane, CRW-1277). A build that installs none runs the workflows that call no `crw.*`
// operation, and refuses to start one that calls one (start.go).
var OpsFor func(ctx context.Context, req LaneRequest) (LaneWorld, error)

// ProductionWorld is the World of the shipped binary: the relay store the run's journal is persisted in, the App Server
// connection of its host, and the allocator of its working directories (<state>/flow/<run>/...). The backend binds every engine
// turn to the run before it starts it, as a resumed run does (agent.BoundConfig). The store is opened for writing; a store that
// does not exist is an error, never a new empty store.
func ProductionWorld(ctx context.Context, req WorldRequest) (World, error) {
	spec := req.Spec
	socket := spec.Socket
	if socket == "" {
		var err error
		if socket, err = store.DefaultSocket(); err != nil {
			return World{}, err
		}
	}
	db := filepath.Join(req.State, "relay.sqlite3")
	if sel, err := store.ResolveStateDir(req.State, socket); err == nil {
		db = sel.DBPath()
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
	if req.Resume {
		if id, err = runRecord(ctx, st, spec.RunID); err != nil {
			_ = st.Close()
			return World{}, err
		}
	}
	threads := agent.NewThreads()
	client := appserver.New(socket, appserver.DefaultBounds, agent.ClientOption(threads))
	var backend *agent.Backend
	lane := LaneWorld{}
	if OpsFor != nil {
		host := hostThreads{BusReader: audit.BusReader{Bus: client.Bus()}, archive: func(ctx context.Context, id string) error { return backend.ArchiveThread(ctx, id) }}
		if lane, err = OpsFor(ctx, LaneRequest{WorldRequest: req, Store: st, Journal: j, Identity: id, Repo: repo, Policy: table, Threads: host}); err != nil {
			_ = client.Close()
			_ = st.Close()
			return World{}, err
		}
	}
	alloc, err := agent.NewAllocator(agent.AllocatorConfig{Repo: repo, StateDir: req.State, RunID: spec.RunID, Base: lane.Base})
	if err != nil {
		_ = client.Close()
		_ = st.Close()
		return World{}, err
	}
	backend, err = agent.New(agent.Config{Client: client, Allocator: alloc, Threads: threads, Bound: &agent.BoundConfig{RunID: spec.RunID, Policy: table}})
	if err != nil {
		_ = client.Close()
		_ = st.Close()
		return World{}, err
	}
	w := World{Journal: j, Backend: backend, Policy: table, Ops: lane.Ops, Prover: lane.Prover, InputSnapshot: lane.InputSnapshot}
	if lane.Journal != nil {
		w.Journal = lane.Journal
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
	// the session nonce the run claims the plan's coordinator epoch under (CRW-1277 e): the run's own id, so a resumed process of the
	// run is the same session and another run is another
	id.EpochNonce = spec.RunID
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

// hostThreads is the host as the milestone close reads and archives the reviewer threads: the turns through the run's client, the archive
// through its backend.
type hostThreads struct {
	audit.BusReader
	archive func(ctx context.Context, id string) error
}

func (h hostThreads) ArchiveThread(ctx context.Context, id string) error { return h.archive(ctx, id) }

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
