package store

import (
	"context"
	"database/sql"
	"fmt"
)

// One recipient's supervisor messages read as a list, and the acknowledgement that parks some of
// them (CRW-1486/S4, findings 6 and 9). Both go through the statements supervisor_sendable.go
// builds, so "unsent" and "never sent" mean here exactly what the channel means by them. The
// acknowledgement adds no hold, state or journal kind: it parks a row under SupervisorHoldSuperseded
// and writes the supervisor_message_superseded row the channel writes when it parks a message itself.

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

// SupervisorListFilter is which of one recipient's messages a list reads: the recipient's (always),
// one project's or every project's (Project empty), in which states (State, SupervisorListUnsent
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
	if f.Project != "" {
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

// AcknowledgeSupervisorMessages parks each of ids under SupervisorHoldSuperseded while nothing of it
// can have gone: unsent, carrying no hold, and with no attempt that may have sent it
// (SupervisorNeverSentSQL). A decision-kind message (an answer is owed to it) and a fault notice
// (whose own channel parks it) are never parked here. Each row parked gets one
// supervisor_message_superseded journal row with detail, which is stored as given. It is one
// transaction, joining the caller's when ctx carries a composing one, and it returns the ids it
// parked, in the order given; an id it did not park is left exactly as it was.
func (s *Store) AcknowledgeSupervisorMessages(ctx context.Context, ids []string, at, detail string) ([]string, error) {
	parked := []string{}
	err := s.Transaction(ctx, func(tx context.Context, _ *sql.Conn) error {
		for _, id := range ids {
			moved, err := s.changedOne(tx, "UPDATE supervisor_messages SET hold_reason = ?, updated_at = ? WHERE message_id = ?"+
				" AND hold_reason IS NULL AND kind <> 'decision' AND obligation_kind <> '"+SupervisorNoticeObligationKind+"' AND "+SupervisorNeverSentSQL(),
				SupervisorHoldSuperseded, at, id)
			if err != nil {
				return err
			}
			if !moved {
				continue
			}
			if _, err := s.exec(tx, "INSERT INTO journal (at, kind, subject, detail) VALUES (?, 'supervisor_message_superseded', ?, ?)", at, id, detail); err != nil {
				return err
			}
			parked = append(parked, id)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return parked, nil
}
