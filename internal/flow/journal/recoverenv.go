package journal

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/appserver"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/backend/agent"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/engine"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// RecoverEnv is engine.RecoverEnv of the shipped binary: it opens the relay store the run's journal is persisted in (the
// state directory the operator named, else the relay's own selection for the control socket) and connects to the App Server
// control socket the run's host listens on. The store is opened for writing, because a recovery appends rows; a store that
// does not exist is an error, never a new empty store. The journal commits every row as it is appended, and Close ends the
// connection, the journal and the store.
func RecoverEnv(ctx context.Context, req engine.RecoverEnvRequest) (engine.RecoverSetup, error) {
	socket := req.Socket
	if socket == "" {
		var err error
		if socket, err = store.DefaultSocket(); err != nil {
			return engine.RecoverSetup{}, err
		}
	}
	selection, err := store.ResolveStateDir(req.State, socket)
	if err != nil {
		return engine.RecoverSetup{}, err
	}
	db := selection.DBPath()
	if _, err := os.Stat(db); err != nil {
		return engine.RecoverSetup{}, fmt.Errorf("there is no relay store at %s (%s): %w", db, selection.Detail, err)
	}
	st, err := store.Open(ctx, db, socket)
	if err != nil {
		return engine.RecoverSetup{}, err
	}
	run := req.RunID
	if run == "" {
		if run, err = RunOf(ctx, st, req.CallID); err != nil {
			_ = st.Close()
			return engine.RecoverSetup{}, err
		}
		if run == "" {
			_ = st.Close()
			return engine.RecoverSetup{}, fmt.Errorf("no run in the store at %s holds call %s", db, req.CallID)
		}
	}
	j, err := Open(ctx, st, run, Identity{}, Options{Immediate: true})
	if err != nil {
		_ = st.Close()
		return engine.RecoverSetup{}, err
	}
	client := appserver.New(socket, appserver.DefaultBounds, agent.ClientOption(agent.NewThreads()))
	host, err := agent.NewHost(agent.Config{Client: client})
	if err != nil {
		_ = client.Close()
		_ = st.Close()
		return engine.RecoverSetup{}, err
	}
	closeAll := func() error {
		host.Close()
		return errors.Join(j.Close(), client.Close(), st.Close())
	}
	return engine.RecoverSetup{Journal: j, Host: host, RunID: run, Close: closeAll}, nil
}
