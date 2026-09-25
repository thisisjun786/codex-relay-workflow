package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

type StageResolution struct {
	Finalized  []string
	Suppressed []string
	Pending    bool
}

// ResolveStaged is resolve_staged: a normal ending finalizes every staged claim on the turn,
// a failed or interrupted one suppresses it, and a running turn leaves it pending. Finalizing
// mints no second event, so the same claim becomes deliverable exactly once.
func (in ReceiptIntake) ResolveStaged(ctx context.Context, turn TurnReference) (StageResolution, error) {
	if turn.Status != "completed" && turn.Status != "failed" && turn.Status != "interrupted" {
		return StageResolution{Pending: true}, nil
	}
	now := in.Now()
	result := StageResolution{Finalized: []string{}, Suppressed: []string{}}
	err := in.Store.Transaction(ctx, func(ctx context.Context, conn *sql.Conn) error {
		events, err := stagedEvents(ctx, conn, turn)
		if err != nil || len(events) == 0 {
			return err
		}
		for _, event := range events {
			if turn.Status == "completed" {
				_, err = conn.ExecContext(ctx, `UPDATE events SET stage=?,finalized_at=?,finalizing_status=? WHERE event_id=?`, StageFinal, now, turn.Status, event)
				result.Finalized = append(result.Finalized, event)
			} else {
				reason := "the turn ended " + turn.Status + ", so the staged claim is not promoted"
				_, err = conn.ExecContext(ctx, `UPDATE events SET stage=?,finalized_at=?,finalizing_status=?,suppressed_reason=? WHERE event_id=?`, StageSuppressed, now, turn.Status, reason, event)
				result.Suppressed = append(result.Suppressed, event)
			}
			if err != nil {
				return fmt.Errorf("settle staged event %q: %w", event, err)
			}
		}
		list := func(ids []string) string {
			quoted := make([]string, len(ids))
			for i, id := range ids {
				quoted[i] = quoteJSON(id)
			}
			return "[" + strings.Join(quoted, ", ") + "]"
		}
		detail := `{"finalized": ` + list(result.Finalized) + `, "suppressed": ` + list(result.Suppressed) + `, "status": ` + quoteJSON(turn.Status) + `}`
		return journal(ctx, conn, "staged_resolved", turn.TurnID, detail, now)
	})
	return result, err
}

func stagedEvents(ctx context.Context, conn *sql.Conn, turn TurnReference) (_ []string, err error) {
	rows, err := conn.QueryContext(ctx, `SELECT event_id FROM events WHERE stage=? AND turn_thread_id=? AND turn_id=? ORDER BY first_seen_at`, StageStaged, turn.ThreadID, turn.TurnID)
	if err != nil {
		return nil, fmt.Errorf("staged claims: %w", err)
	}
	defer func() {
		if closeErr := rows.Close(); err == nil && closeErr != nil {
			err = fmt.Errorf("close staged rows: %w", closeErr)
		}
	}()
	var events []string
	for rows.Next() {
		var event string
		if err := rows.Scan(&event); err != nil {
			return nil, fmt.Errorf("staged event: %w", err)
		}
		events = append(events, event)
	}
	return events, rows.Err()
}
