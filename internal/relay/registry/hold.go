package registry

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
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

// RegisterUnderHold is intent.register's relay-side decision: take the store's write lock,
// decide the dispatch's generation under it, and run publish (naming that generation) before the
// lock is released, so an advance either committed before (stale) or waits for the publication.
func RegisterUnderHold(ctx context.Context, dbPath, rid, dispatch string, publish func(generation int64) error) error {
	hold, unavailable := store.HoldForWrite(ctx, dbPath, SQLiteTimeout)
	if hold == nil {
		return refuse(contract.RefusalUnregisteredRelationship, "the relay store could not be held for this registration, so it cannot be "+
			"confirmed that relationship %s is still this assignment's at the moment the registration lands (%s); it is refused "+
			"rather than published on the caller's word", rid, unavailable)
	}
	defer hold.Release()
	state, generation := generationState(ctx, hold.Conn, rid, dispatch)
	switch state {
	case "":
		return refuse(contract.RefusalUnregisteredRelationship, "the relay store could not be read, so it cannot be confirmed that relationship "+
			"%s belongs to this assignment; registration is refused rather than taken on the caller's word", rid)
	case DispatchStale:
		return refuse(contract.RefusalStaleGeneration, "this assignment's dispatch request id opened an earlier generation of "+
			"relationship %s, which has since advanced, so registering it would attribute the current generation's receipts to a superseded assignment", rid)
	case DispatchAbsent:
		return refuse(contract.RefusalRelationshipConflict, "the relay has no generation of relationship %s opened under "+
			"this assignment's dispatch request id, so it is not this assignment's relationship", rid)
	}
	return publish(generation)
}
