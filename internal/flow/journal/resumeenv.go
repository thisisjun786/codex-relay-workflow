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

// ResumeEnv is engine.ResumeEnv of the shipped binary: it opens the relay store the run's journal is persisted in (the state
// directory the operator named, else the relay's own selection for the control socket), connects to the App Server control
// socket the run's host listens on, and makes the allocator of the run's working directories (<state>/flow/<run>/...) over the
// repository the operator named, else the one the working directory is in. The store is opened for writing, because a resumed
// run appends rows; a store that does not exist is an error, never a new empty store. Close commits the rows still buffered
// and ends the connection and the store.
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
	backend, err := agent.New(agent.Config{Client: client, Allocator: alloc, Threads: threads})
	if err != nil {
		_ = client.Close()
		_ = st.Close()
		return engine.ResumeSetup{}, err
	}
	closeAll := func() error {
		backend.Close()
		return errors.Join(j.Close(), client.Close(), st.Close())
	}
	return engine.ResumeSetup{Journal: j, Backend: backend, Close: closeAll}, nil
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
