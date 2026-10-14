package journal

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/appserver"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/backend/agent"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/engine"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// UpwardFactory makes the hook the resumed run hands what goes up from it to; the binary installs it (internal/flow/cli). Nil installs none.
// state and socket are the relay the store was selected by (the state directory the run was resumed in, the socket of its host).
var UpwardFactory func(st *store.Store, j *Journal, runID, state, socket string) engine.UpwardHook

// ResumeEnv is engine.ResumeEnv of the shipped binary. A run its started row records on the session backend gets its session
// environment (SessionEnv: the owner thread of its record); any other the App Server backend. It opens the relay store the run's journal is persisted in (the state
// directory the operator named, else the relay's own selection for the control socket), connects to the App Server control
// socket the run's host listens on, and makes the allocator of the run's working directories (<state>/flow/<run>/...) over the
// repository the operator named, else the one the working directory is in. The backend writes the flow binding of every
// engine turn before turn/start, as the first process of a run on the App Server does (agent.BoundConfig). The store is opened
// for writing, because a resumed run appends rows; a store that does not exist is an error, never a new empty store. Close
// commits the rows still buffered and ends the connection and the store.
func ResumeEnv(ctx context.Context, req engine.ResumeEnvRequest) (engine.ResumeSetup, error) {
	socket := req.Socket
	if socket == "" {
		var err error
		if socket, err = store.DefaultSocket(); err != nil {
			return engine.ResumeSetup{}, err
		}
	}
	selection, err := store.ResolveStateDir(req.State, socket)
	if err != nil {
		return engine.ResumeSetup{}, err
	}
	db := selection.DBPath()
	if _, err := os.Stat(db); err != nil {
		return engine.ResumeSetup{}, fmt.Errorf("there is no relay store at %s (%s): %w", db, selection.Detail, err)
	}
	repo, err := repository(ctx, req.Repo)
	if err != nil {
		return engine.ResumeSetup{}, err
	}
	// The turns of a run on the App Server go to the App Server, so the resumed process binds every engine turn as its first process does
	// (CRW-1207 F14): the run id and the role table of the run (the one `crw flow resume` pins, read from the same policy
	// source) make the binding, and a line that cannot be written holds the call host_config_flow_binding with nothing sent.
	policy, _, err := engine.LoadPolicy()
	if err != nil {
		return engine.ResumeSetup{}, err
	}
	st, err := store.Open(ctx, db, socket)
	if err != nil {
		return engine.ResumeSetup{}, err
	}
	j, err := Open(ctx, st, req.RunID, Identity{}, Options{})
	if err != nil {
		_ = st.Close()
		return engine.ResumeSetup{}, err
	}
	alloc, err := agent.NewAllocator(agent.AllocatorConfig{Repo: repo, StateDir: selection.Path, RunID: req.RunID})
	if err != nil {
		_ = st.Close()
		return engine.ResumeSetup{}, err
	}
	threads := agent.NewThreads()
	client := appserver.New(socket, appserver.DefaultBounds, agent.ClientOption(threads))
	// the backend the run's started row records (CRW-1316 S3): a session run goes on with its session environment, and its App
	// Server backend starts no thread and no turn, so it writes no engine line and needs no explorer pair (an undecided part,
	// CRW-1331)
	recorded, _ := engine.StartedBackend(j.Events(), req.RunID)
	var binding *agent.BoundConfig
	if recorded != engine.BackendSession {
		binding = &agent.BoundConfig{RunID: req.RunID, Policy: policy}
	}
	backend, err := agent.New(agent.Config{Client: client, Allocator: alloc, Threads: threads, Bound: binding})
	if err != nil {
		_ = client.Close()
		_ = st.Close()
		return engine.ResumeSetup{}, err
	}
	closeAll := func() error {
		backend.Close()
		return errors.Join(j.Close(), client.Close(), st.Close())
	}
	setup := engine.ResumeSetup{Journal: j, Backend: backend, Close: closeAll}
	if recorded == engine.BackendSession {
		owner, err := runOwner(ctx, st, req.RunID)
		if err == nil {
			cwd, _ := os.Getwd()
			var sb *agent.SessionBackend
			if sb, setup.Session, err = SessionEnv(SessionEnvRequest{Store: st, Client: client, Allocator: alloc, StateDir: selection.Path, RunID: req.RunID, Owner: owner, OwnerCwd: cwd, Journal: j}); err == nil {
				setup.Backend = sb
			}
		}
		if err != nil {
			_ = closeAll()
			return engine.ResumeSetup{}, fmt.Errorf("the session environment of run %s: %w", req.RunID, err)
		}
	}
	if UpwardFactory != nil {
		setup.Upward = UpwardFactory(st, j, req.RunID, selection.Path, socket)
	}
	return setup, nil
}

// runOwner is the owner thread a run's record names (flow_runs.actor_task_id).
func runOwner(ctx context.Context, st *store.Store, run string) (string, error) {
	var owner string
	if err := st.DB.QueryRowContext(ctx, `SELECT COALESCE(actor_task_id, '') FROM flow_runs WHERE run_id = ?`, run).Scan(&owner); err != nil {
		return "", fmt.Errorf("the run record of %s: %w", run, err)
	}
	return owner, nil
}

// repository is the git repository a run's worktrees are made from: the directory the operator named, else the top level of
// the repository the working directory is in. It is an absolute path.
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
