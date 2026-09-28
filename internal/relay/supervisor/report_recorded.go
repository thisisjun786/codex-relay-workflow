package supervisor

import (
	"context"
	"database/sql"
	"errors"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/evidence"
)

// RecordReport records a produced report, not a delivery. One obligation gets one
// journal entry even when two recorders race or a later call supplies a new message.
func (c *Channel) RecordReport(ctx context.Context, o Obligation, at string, messageID, note *string) (map[string]any, error) {
	var seq int64
	recorded := false
	err := c.Store.Transaction(ctx, func(ctx context.Context, conn *sql.Conn) error {
		err := conn.QueryRowContext(ctx, "SELECT seq FROM journal WHERE kind = 'supervisor_report' AND subject = ? LIMIT 1", o.ID).Scan(&seq)
		if err == nil {
			return nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		var message any
		if messageID != nil {
			message = *messageID
		}
		text := ""
		if note != nil {
			text = *note
		}
		detail := evidence.Dumps(contract.OrderedObject{{Key: "kind", Value: o.Kind}, {Key: "relationId", Value: o.RelationID}, {Key: "subject", Value: o.Subject}, {Key: "messageId", Value: message}, {Key: "note", Value: text}}, false, false, true)
		if _, err = conn.ExecContext(ctx, "INSERT INTO journal (at, kind, subject, detail) VALUES (?,?,?,?)", at, "supervisor_report", o.ID, string(detail)); err != nil {
			return err
		}
		if err = conn.QueryRowContext(ctx, "SELECT last_insert_rowid()").Scan(&seq); err != nil {
			return err
		}
		recorded = true
		return nil
	})
	if err != nil {
		return nil, err
	}
	return map[string]any{"recorded": recorded, "seq": seq}, nil
}
