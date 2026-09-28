package supervisor

import (
	"context"
	"database/sql"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/delivery"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/evidence"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// A busy recipient never reaches the transport. Count deferrals only when the
// compare-and-set of that recipient's current row succeeds, under the write lock.
func (c *Channel) deferBusy(ctx context.Context, row store.SupervisorMessagesRow, now float64) error {
	at := delivery.ISOOf(now)
	return c.Store.Transaction(ctx, func(tx context.Context, _ *sql.Conn) error {
		var seen int64
		err := c.Store.Q(tx).QueryRowContext(tx, "SELECT COUNT(*) FROM journal WHERE kind='supervisor_message_deferred' AND subject=? AND seq>(SELECT COALESCE(MAX(seq),0) FROM journal WHERE kind='supervisor_message_readdressed' AND subject=?)", row.MessageID, row.MessageID).Scan(&seen)
		if err != nil {
			return err
		}
		next := seen + 1
		policy := delivery.DefaultPolicy()
		hold := ""
		if next >= policy.BusyMaxAttempts {
			hold = "busy_cap"
		}
		var holdValue any
		if hold != "" {
			holdValue = hold
		}
		var previousHold, previousEligible any
		if row.HoldReason.Valid {
			previousHold = row.HoldReason.String
		}
		if row.NextEligibleAt.Valid {
			previousEligible = row.NextEligibleAt.Float64
		}
		result, err := c.Store.Q(tx).ExecContext(tx, "UPDATE supervisor_messages SET state='deferred_busy',next_eligible_at=?,hold_reason=?,updated_at=? WHERE message_id=? AND state=? AND hold_reason IS ? AND next_eligible_at IS ? AND recipient_task_id=? AND attempt_count=?", now+policy.DelayFor(next, "busy"), holdValue, at, row.MessageID, row.State, previousHold, previousEligible, row.RecipientTaskID, row.AttemptCount)
		if err != nil {
			return err
		}
		changed, err := result.RowsAffected()
		if err != nil || changed == 0 {
			return err
		}
		detail := evidence.Dumps(contract.OrderedObject{{Key: "deferral", Value: next}, {Key: "holdReason", Value: holdValue}}, false, false, true)
		_, err = c.Store.Q(tx).ExecContext(tx, "INSERT INTO journal(at,kind,subject,detail) VALUES(?,'supervisor_message_deferred',?,?)", at, row.MessageID, string(detail))
		return err
	})
}
