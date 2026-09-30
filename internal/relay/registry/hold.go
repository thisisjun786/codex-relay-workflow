package registry

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// The store half of a marker registration (intent.py registration_hold, _generation_state,
// dispatch_generation_state). The marker files themselves are intent.py's and are ported with
// it (todo 21); publish is the caller's write, run while the hold is taken.

// Dispatch generation states (intent.DISPATCH_*).
const (
	DispatchCurrent = "current"
	DispatchStale   = "stale"
	DispatchAbsent  = "absent"
)

// SQLiteTimeout is intent.SQLITE_TIMEOUT.
const SQLiteTimeout = 2 * time.Second

// generationState is intent._generation_state; "" state means the read itself failed.
func generationState(ctx context.Context, q store.Querier, rid, dispatch string) (string, int64) {
	var opened, current int64
	err := q.QueryRowContext(ctx, "SELECT g.execution_generation AS opened, r.execution_generation AS current  FROM generations g"+
		"  JOIN relationships r ON r.relationship_id = g.relationship_id WHERE g.relationship_id = ? AND g.dispatch_request_id = ?",
		rid, dispatch).Scan(&opened, &current)
	if errors.Is(err, sql.ErrNoRows) {
		return DispatchAbsent, 0
	}
	if err != nil {
		return "", 0
	}
	if opened != current {
		return DispatchStale, current
	}
	return DispatchCurrent, current
}

// DispatchGenerationState is intent.dispatch_generation_state: (state, readable), read-only.
func DispatchGenerationState(ctx context.Context, dbPath, rid, dispatch string) (string, bool) {
	s, err := store.OpenReadOnly(ctx, dbPath, SQLiteTimeout)
	if err != nil {
		return DispatchAbsent, false
	}
	defer s.Close()
	state, _ := generationState(ctx, s, rid, dispatch)
	if state == "" {
		return DispatchAbsent, false
	}
	return state, true
}
