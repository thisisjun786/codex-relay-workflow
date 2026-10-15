// Package report is the upward half of a flow run (CRW-1204, F11): the rows a run and its commands write into flow_events for what goes
// up to the orchestrator and the initiative lead (milestone reports, project blockers, decision requests, their deliveries and their
// reading, the plan summary notices), the recipient of a report, and the library `crw flow report-up`, `reports`, `report-ack` and
// `report-delivered` work through.
//
// Every row here is a flow_events row. A row the run process writes is appended to the run's own journal (the sequence number is the
// journal's); a row a command writes (a process that is not the run, possibly while the run is alive) takes its sequence number from the
// range that starts at ForeignSeqBase, so it can never meet a number the run's journal hands out.
package report

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/flow/engine"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// The row kinds this package writes (flow_events.kind).
const (
	// KindReport is the report row: what goes up, to whom, and the digest of its body. Written before any delivery.
	KindReport = "report"
	// KindDelivery is a delivery row: the result (accepted, queued, refused, unknown, standing) appended to a report row.
	KindDelivery = "report_delivery"
	// KindRead is the lead's reading of a report.
	KindRead = "report_read"
	// KindSummaryNotice is the intent to tell the orchestrator thread that a summary entry is owed, once for each summary id.
	KindSummaryNotice = "summary_notice"
	// KindSummarySettled is the result of that notice (accepted, queued, obsolete, refused).
	KindSummarySettled = "summary_notice_settled"
	// KindSummaryDocument records the document a run keeps the plan summary in (crw flow run --summary-document).
	KindSummaryDocument = "summary_document"
	// KindEscalationNotice records that an escalation row of the run was told to the orchestrator thread.
	KindEscalationNotice = "escalation_notice"
	// KindCompletionNotice records the completion digest told to the orchestrator thread: notice_key, status (the engine status), result
	// and request_id, and, when the run's finished row carries an outcome (engine.Outcome, CRW-1487), its state, its reason and the lanes
	// that did not land (node:state/reason).
	KindCompletionNotice = "completion_notice"
)

// The report kinds (the report_kind member of a report row).
const (
	KindMilestoneReport        = "milestone_report"
	KindProjectBlocked         = "project_blocked"
	KindProjectDecisionRequest = "project_decision_request"
	KindJunDecisionRequest     = "jun_decision_request"
)

// ForeignSeqBase is the first sequence number a command (a process that is not the run) gives a flow_events row. The journal of a run
// counts up from 1; a command's rows count up from here, so a command can write into a run that is alive and a resumed journal
// carries on after both.
const ForeignSeqBase = engine.ForeignSeqBase

// DB is what the readers need: *sql.DB, *sql.Conn-backed queriers of a store, and *store.ReadOnly all satisfy it.
type DB interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// Row is one flow_events row as the readers see it.
type Row struct {
	RowID      int64
	Run        string
	Seq        int64
	Kind       string
	CallID     string
	Node       string
	Subject    string
	Payload    map[string]any
	RecordedAt string
}

// Str reads a text member of the payload.
func (r Row) Str(key string) string {
	switch v := r.Payload[key].(type) {
	case string:
		return v
	case float64:
		return fmt.Sprint(int64(v))
	case bool:
		return fmt.Sprint(v)
	}
	return ""
}

// NewRow is a row to write.
type NewRow struct {
	Kind    string
	Subject string
	Node    string
	Payload map[string]any
}

// Appender writes rows into the flow_events of one run. The run's own journal appends through it as the run process; a command appends
// through ForeignAppender.
type Appender interface {
	// Append writes the row and returns its sequence number.
	Append(ctx context.Context, row NewRow) (int64, error)
	// AppendOnce writes the row unless a row of its kind already carries payload[key] equal to value (in any run), and reports whether it wrote.
	// The check and the write are one transaction where the appender has one.
	AppendOnce(ctx context.Context, row NewRow, key string) (written bool, seq int64, err error)
}

// ForeignAppender writes rows of a command into the flow_events of an existing run.
type ForeignAppender struct {
	Store *store.Store
	Run   string
	Now   func() time.Time
}

func (a ForeignAppender) now() time.Time {
	if a.Now != nil {
		return a.Now()
	}
	return time.Now()
}

// Append implements Appender.
func (a ForeignAppender) Append(ctx context.Context, row NewRow) (int64, error) {
	var seq int64
	err := a.Store.Transaction(ctx, func(ctx context.Context, conn *sql.Conn) error {
		var err error
		seq, err = a.insert(ctx, conn, row)
		return err
	})
	return seq, err
}

// AppendOnce implements Appender: the key is a member of the payload that identifies the row's subject (a summary id, a report id and result).
func (a ForeignAppender) AppendOnce(ctx context.Context, row NewRow, key string) (bool, int64, error) {
	value, _ := row.Payload[key].(string)
	if value == "" {
		return false, 0, fmt.Errorf("report: a row written once needs a text member %q", key)
	}
	var (
		seq     int64
		written bool
	)
	err := a.Store.Transaction(ctx, func(ctx context.Context, conn *sql.Conn) error {
		var n int
		if err := conn.QueryRowContext(ctx, `SELECT COUNT(*) FROM flow_events WHERE kind = ? AND json_extract(payload_json, '$.'||?) = ?`, row.Kind, key, value).Scan(&n); err != nil {
			return err
		}
		if n > 0 {
			return nil
		}
		var err error
		seq, err = a.insert(ctx, conn, row)
		written = err == nil
		return err
	})
	return written, seq, err
}

func (a ForeignAppender) insert(ctx context.Context, conn *sql.Conn, row NewRow) (int64, error) {
	var exists int
	if err := conn.QueryRowContext(ctx, `SELECT COUNT(*) FROM flow_runs WHERE run_id = ?`, a.Run).Scan(&exists); err != nil {
		return 0, err
	}
	if exists == 0 {
		return 0, fmt.Errorf("report: the store holds no run %q", a.Run)
	}
	var top sql.NullInt64
	if err := conn.QueryRowContext(ctx, `SELECT MAX(seq) FROM flow_events WHERE run_id = ? AND seq >= ?`, a.Run, ForeignSeqBase).Scan(&top); err != nil {
		return 0, err
	}
	seq := ForeignSeqBase
	if top.Valid {
		seq = top.Int64 + 1
	}
	payload := map[string]any{"_kind": "flow"}
	for k, v := range row.Payload {
		if strings.HasPrefix(k, "_") {
			return 0, fmt.Errorf("report: a payload member never starts with an underscore (%q)", k)
		}
		payload[k] = v
	}
	if row.Subject != "" {
		payload["_label"] = row.Subject
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return 0, err
	}
	_, err = conn.ExecContext(ctx, `INSERT INTO flow_events (run_id, seq, kind, call_id, node_id, subject, payload_json, recorded_at) VALUES (?,?,?,?,?,?,?,?)`,
		a.Run, seq, row.Kind, "", row.Node, row.Subject, string(raw), a.now().UTC().Format(time.RFC3339Nano))
	return seq, err
}

// ReadRows reads flow_events rows of the kinds given (all kinds when none), of one run (all runs when run is empty), in the order they were
// written.
func ReadRows(ctx context.Context, db DB, run string, kinds ...string) ([]Row, error) {
	q := `SELECT rowid, run_id, seq, kind, call_id, node_id, subject, payload_json, recorded_at FROM flow_events`
	var where []string
	var args []any
	if run != "" {
		where = append(where, `run_id = ?`)
		args = append(args, run)
	}
	if len(kinds) > 0 {
		marks := make([]string, len(kinds))
		for i, k := range kinds {
			marks[i] = "?"
			args = append(args, k)
		}
		where = append(where, `kind IN (`+strings.Join(marks, ",")+`)`)
	}
	if len(where) > 0 {
		q += ` WHERE ` + strings.Join(where, " AND ")
	}
	rows, err := db.QueryContext(ctx, q+` ORDER BY rowid`, args...)
	if err != nil {
		return nil, fmt.Errorf("report: %w", err)
	}
	defer rows.Close()
	var out []Row
	for rows.Next() {
		var r Row
		var payload string
		if err := rows.Scan(&r.RowID, &r.Run, &r.Seq, &r.Kind, &r.CallID, &r.Node, &r.Subject, &payload, &r.RecordedAt); err != nil {
			return nil, fmt.Errorf("report: %w", err)
		}
		r.Payload = map[string]any{}
		if strings.TrimSpace(payload) != "" {
			if err := json.Unmarshal([]byte(payload), &r.Payload); err != nil {
				return nil, fmt.Errorf("report: the row %d of run %s: %w", r.Seq, r.Run, err)
			}
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// RunInfo is the run record (flow_runs).
type RunInfo struct {
	RunID, Workflow, PlanID, Actor, StartedAt string
	PlanRevision                              int64
}

// ReadRun reads one run record.
func ReadRun(ctx context.Context, db DB, run string) (RunInfo, bool, error) {
	var r RunInfo
	err := db.QueryRowContext(ctx, `SELECT run_id, workflow, plan_id, plan_revision, actor_task_id, started_at FROM flow_runs WHERE run_id = ?`, run).
		Scan(&r.RunID, &r.Workflow, &r.PlanID, &r.PlanRevision, &r.Actor, &r.StartedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return RunInfo{}, false, nil
	}
	return r, err == nil, err
}
