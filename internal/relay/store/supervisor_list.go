package store

import (
	"context"
	"fmt"
)

// One recipient's supervisor messages read as a list (CRW-1486/S4, finding 9). It goes through the
// statements supervisor_sendable.go builds, so "unsent" means here exactly what the channel means by
// it.

// The states a list reads.
const (
	// SupervisorListUnsent is every unsent message (SupervisorUnsentSQL), the default.
	SupervisorListUnsent = "unsent"
	// SupervisorListPending is every message no send of which has settled: unsent, or claimed and
	// not yet on the transport (SupervisorRestatableSQL).
	SupervisorListPending = "pending"
	// SupervisorListAll is every message, whatever its state.
	SupervisorListAll = "all"
)

// SupervisorListHoldNone is the hold filter for a message that carries no hold.
const SupervisorListHoldNone = "none"

// SupervisorListProjectNone is the project filter, and the project count key, for a message with
// no project (CRW-1500). It also names a project registered as none, so the filter and the count
// read the same set.
const SupervisorListProjectNone = "none"

// SupervisorListFilter is which of one recipient's messages a list reads: the recipient's (always),
// one project's, every project's (Project empty) or, for SupervisorListProjectNone, the messages
// with no project and those of a project named none, in which states (State, SupervisorListUnsent
// when empty), and carrying which hold (Hold: empty for any, SupervisorListHoldNone for none, or the
// hold word).
type SupervisorListFilter struct {
	Recipient, Project, State, Hold string
}

// ListSupervisorMessages is the recipient's messages the filter names, in the order the channel
// sends them: staging time, then message id. It only reads.
func (s *Store) ListSupervisorMessages(ctx context.Context, f SupervisorListFilter) ([]SupervisorMessagesRow, error) {
	query := "SELECT " + supervisorMessagesColumns + " FROM supervisor_messages WHERE recipient_task_id = ?"
	args := []any{f.Recipient}
	switch f.State {
	case "", SupervisorListUnsent:
		query += " AND " + SupervisorUnsentSQL("")
	case SupervisorListPending:
		query += " AND " + SupervisorRestatableSQL("")
	case SupervisorListAll:
	default:
		return nil, fmt.Errorf("a message list of the 부모 queue reads the %s, %s or %s messages, not %q", SupervisorListUnsent, SupervisorListPending, SupervisorListAll, f.State)
	}
	switch f.Project {
	case "":
	case SupervisorListProjectNone:
		query += " AND (project_key IS NULL OR project_key = ?)"
		args = append(args, SupervisorListProjectNone)
	default:
		query += " AND project_key = ?"
		args = append(args, f.Project)
	}
	switch f.Hold {
	case "":
	case SupervisorListHoldNone:
		query += " AND hold_reason IS NULL"
	default:
		query += " AND hold_reason = ?"
		args = append(args, f.Hold)
	}
	return queryRows(ctx, s, scanSupervisorMessages, query+" ORDER BY staged_at, message_id", args...)
}
