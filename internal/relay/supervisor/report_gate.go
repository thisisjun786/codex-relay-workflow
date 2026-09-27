package supervisor

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// AssertReportResubmission is report.py's correction gate. Run it with the
// transaction body's ctx when recording; an attempt's transport start and this
// decision must serialize on the store's single SQLite connection.
func AssertReportResubmission(ctx context.Context, s *store.Store, eventID string, submission int64) error {
	frozen, err := s.One(ctx, `SELECT m.message_id,m.submission_no FROM supervisor_messages m WHERE m.event_id = ? AND EXISTS (SELECT 1 FROM supervisor_attempts a WHERE a.message_id=m.message_id AND a.transport_started_at IS NOT NULL AND NOT (a.send_attempted='no' AND a.retry_safe=1)) ORDER BY m.staged_at LIMIT 1`, eventID)
	if err != nil {
		return err
	}
	if frozen != nil {
		return reportRefusal("malformed_receipt", fmt.Sprintf("a supervisor report about this event was sent (message %v, composed from submission %v), and its evidence points at this event; a changed report would leave those bytes saying one thing while their evidence says another, so this report no longer changes. A correction the level above needs is a new fact, reported as one", frozen.Get("message_id"), frozen.Get("submission_no")))
	}
	attempts, err := s.All(ctx, `SELECT a.record,a.state,x.submission_no FROM attempts a LEFT JOIN attempt_report_submissions x ON x.request_id=a.request_id WHERE a.event_id = ?`, eventID)
	if err != nil {
		return err
	}
	delivered := int64(0)
	legacy := false
	for _, row := range attempts {
		reached := row.Get("state") == "inbox_only"
		if record, ok := row.Get("record").(string); ok {
			var parsed map[string]any
			if json.Unmarshal([]byte(record), &parsed) != nil || parsed["sendAttempted"] != "no" {
				reached = true
			}
		} else {
			reached = true
		}
		if !reached {
			continue
		}
		if row.Get("submission_no") == nil {
			legacy = true
			if delivered < 1 {
				delivered = 1
			}
		} else if n := row.Get("submission_no").(int64); n > delivered {
			delivered = n
		}
	}
	var highest sql.NullInt64
	if err := s.Q(ctx).QueryRowContext(ctx, "SELECT MAX(submission_no) FROM work_reports WHERE event_id = ?", eventID).Scan(&highest); err != nil {
		return err
	}
	maxStored := highest.Int64
	if submission >= maxStored && submission > delivered {
		return nil
	}
	if submission < maxStored {
		return reportRefusal("malformed_receipt", fmt.Sprintf("submission %d of this report already exists, and both reading and delivery take the highest, so recording %d would report success and change nothing anyone sees. Correct submission %d, or record %d", maxStored, submission, maxStored, max(maxStored, delivered)+1))
	}
	if legacy && delivered == 1 {
		return reportRefusal("malformed_receipt", "a pre-contract message has already been delivered for this event, so a first report would change what a retry says without changing what it calls itself; record it as submission 2 or higher")
	}
	return reportRefusal("malformed_receipt", fmt.Sprintf("submission %d of this report has already been frozen into a delivered attempt, so replacing it in place would change what a retry says without changing what it calls itself; record this as submission %d or higher. A submission that has never been sent can still be corrected in place", delivered, max(maxStored, delivered)+1))
}
