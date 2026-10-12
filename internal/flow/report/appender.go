package report

import (
	"context"
	"fmt"

	"github.com/thisisjun786/codex-relay-workflow/internal/flow/engine"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// JournalAppender writes the rows of a run process into the run's own journal: the sequence number is the journal's, and a row is committed
// before Append returns, so a report row is on record before the delivery that follows it.
type JournalAppender struct {
	Journal engine.Journal
	RunID   string
	// Store, when set, is also asked whether a row written once exists already in any run (a summary id has one notice however many runs
	// reconcile it).
	Store *store.Store
}

// Append implements Appender.
func (a JournalAppender) Append(_ context.Context, row NewRow) (int64, error) {
	data := map[string]any{}
	for k, v := range row.Payload {
		data[k] = v
	}
	if row.Node != "" {
		data["node"] = row.Node
	}
	ev := a.Journal.Append(engine.Event{RunID: a.RunID, Kind: engine.KindFlow, Event: row.Kind, Label: row.Subject, Data: data})
	if f, ok := a.Journal.(interface{ Flush() error }); ok {
		if err := f.Flush(); err != nil {
			return int64(ev.Seq), err
		}
	}
	if f, ok := a.Journal.(engine.FailingJournal); ok {
		if err := f.Err(); err != nil {
			return int64(ev.Seq), fmt.Errorf("report: the journal cannot store the row: %w", err)
		}
	}
	return int64(ev.Seq), nil
}

// AppendOnce implements Appender.
func (a JournalAppender) AppendOnce(ctx context.Context, row NewRow, key string) (bool, int64, error) {
	value, _ := row.Payload[key].(string)
	if value == "" {
		return false, 0, fmt.Errorf("report: a row written once needs a text member %q", key)
	}
	for _, ev := range a.Journal.Events() {
		if ev.Kind == engine.KindFlow && ev.Event == row.Kind && fmt.Sprint(ev.Data[key]) == value {
			return false, 0, nil
		}
	}
	if a.Store != nil {
		var n int
		if err := a.Store.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM flow_events WHERE kind = ? AND json_extract(payload_json, '$.'||?) = ?`, row.Kind, key, value).Scan(&n); err != nil {
			return false, 0, err
		}
		if n > 0 {
			return false, 0, nil
		}
	}
	seq, err := a.Append(ctx, row)
	return err == nil, seq, err
}

var (
	_ Appender = JournalAppender{}
	_ Appender = ForeignAppender{}
)
