package cli

import (
	"context"
	"os"
	"syscall"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/flow/engine"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// Deps is everything the commands reach outside the process. The binary runs on Default(); a test replaces what it fakes.
type Deps struct {
	Now    func() time.Time
	Getenv func(string) (string, bool)
	// Reader opens the relay store read-only.
	Reader func(ctx context.Context, state, socket string) (*Reader, error)
	// Writer opens the relay store for writing, with the state directory it is in (`crw flow answer`).
	Writer func(ctx context.Context, state, socket string) (*store.Store, string, error)
	// Spawn starts the detached process of a run and returns its pid.
	Spawn func(ctx context.Context, req SpawnRequest) (int, error)
	// Probe judges the five preflight facts.
	Probe Prober
	// Nodes lists the nodes a run will work on, for the preview; nil lists none (a workflow that reads its plan when it runs).
	Nodes func(ctx context.Context, spec Spec, src Source) (Preview, error)
	// Signal sends a signal to the process that holds a run lock.
	Signal func(pid int, sig syscall.Signal) error
	// World opens what the process of a run works on: its journal, its backend and its operations.
	World WorldFunc
	// Ready waits until the process of a run holds the run lock, or says why it does not; nil does not wait.
	Ready func(ctx context.Context, state, run string, pid int) error
}

// Default is the Deps of the shipped binary.
func Default() *Deps {
	return &Deps{
		Now:    time.Now,
		Getenv: os.LookupEnv,
		Reader: OpenReader,
		Writer: OpenWriter,
		Spawn:  SpawnDetached,
		Probe:  StoreProber{},
		Signal: func(pid int, sig syscall.Signal) error { return syscall.Kill(pid, sig) },
		World:  ProductionWorld,
		Ready:  AwaitLock,
	}
}

// OpenWriter opens the relay store for writing; a store that does not exist is an error, never a new empty store.
func OpenWriter(ctx context.Context, state, socket string) (*store.Store, string, error) {
	sel, err := ResolveState(state, socket)
	if err != nil {
		return nil, "", err
	}
	db := sel.DBPath()
	if _, err := os.Stat(db); err != nil {
		return nil, "", err
	}
	if socket == "" {
		if socket, err = store.DefaultSocket(); err != nil {
			return nil, "", err
		}
	}
	st, err := store.Open(ctx, db, socket)
	if err != nil {
		return nil, "", err
	}
	return st, sel.Path, nil
}

// World is what the process of a run works on.
type World struct {
	Journal       engine.Journal
	Backend       engine.Backend
	Ops           map[string]engine.Op
	Prover        engine.Prover
	InputSnapshot func(ctx context.Context, req engine.SnapshotRequest) (string, error)
	// Policy is the role table; nil reads the execution policy file the environment names.
	Policy *engine.Policy
	// Close commits the rows still buffered and ends the connections.
	Close func() error
}

// WorldRequest is what a World is opened for.
type WorldRequest struct {
	Spec Spec
	// State is the relay state directory.
	State string
	// Resume is true when the run goes on from a journal an earlier process wrote.
	Resume bool
}

// WorldFunc opens a World.
type WorldFunc func(ctx context.Context, req WorldRequest) (World, error)
