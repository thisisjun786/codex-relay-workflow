package cli

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"

	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/appserver"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/audit"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/engine"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/journal"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// OpenAuditStore is AuditEnv of the shipped binary: it opens the relay store the journal of the run is persisted in (the state directory the
// operator named, else the relay's own selection for the control socket) and connects to the App Server control socket the run's
// host listens on, to read the reviewer threads. The store is opened for writing, because the result of the audit is a row of the
// journal; a store that does not exist is an error, never a new empty store. An acceptance given without its run is looked up in
// the store.
func OpenAuditStore(ctx context.Context, req AuditRequest) (AuditSetup, error) {
	socket := req.Socket
	if socket == "" {
		var err error
		if socket, err = store.DefaultSocket(); err != nil {
			return AuditSetup{}, err
		}
	}
	selection, err := store.ResolveStateDir(req.State, socket)
	if err != nil {
		return AuditSetup{}, err
	}
	db := selection.DBPath()
	if _, err := os.Stat(db); err != nil {
		return AuditSetup{}, fmt.Errorf("there is no relay store at %s (%s): %w", db, selection.Detail, err)
	}
	st, err := store.Open(ctx, db, socket)
	if err != nil {
		return AuditSetup{}, err
	}
	run := req.RunID
	if run == "" {
		if run, err = RunOfAcceptance(ctx, st, req.AcceptanceID); err != nil {
			_ = st.Close()
			return AuditSetup{}, err
		}
	}
	j, err := journal.Open(ctx, st, run, journal.Identity{}, journal.Options{})
	if err != nil {
		_ = st.Close()
		return AuditSetup{}, err
	}
	client := appserver.New(socket, appserver.DefaultBounds)
	return AuditSetup{RunID: run, Journal: j, Reader: audit.BusReader{Bus: client.Bus()}, Finished: runEnded(j.Events()), Close: func() error {
		return errors.Join(j.Close(), client.Close(), st.Close())
	}}, nil
}

// runEnded says whether a process of the run ended it: a finished row, or the run/paused row of a run the operator paused
// (CRW-1487), which leaves the journal to nobody as a finished one does.
func runEnded(events []engine.Event) bool {
	for _, ev := range events {
		if ev.Kind == engine.KindRun && (ev.Event == "finished" || ev.Event == "paused") {
			return true
		}
	}
	return false
}

// RunOfAcceptance is the run that holds an acceptance, searched in the rows of the store. An acceptance that no run holds, or that
// more than one run holds, is an error: the operator names the run.
func RunOfAcceptance(ctx context.Context, st *store.Store, acceptance string) (string, error) {
	rows, err := st.DB.QueryContext(ctx, `SELECT DISTINCT run_id FROM flow_calls WHERE acceptance_id = ? AND kind = 'crw.accept'`, acceptance)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	var runs []string
	for rows.Next() {
		var run sql.NullString
		if err := rows.Scan(&run); err != nil {
			return "", err
		}
		if run.String != "" {
			runs = append(runs, run.String)
		}
	}
	if err := rows.Err(); err != nil {
		return "", err
	}
	switch len(runs) {
	case 0:
		return "", fmt.Errorf("no run in the store holds the acceptance %s", acceptance)
	case 1:
		return runs[0], nil
	}
	return "", fmt.Errorf("the acceptance %s is held by %d runs (%v): name one with --run", acceptance, len(runs), runs)
}
