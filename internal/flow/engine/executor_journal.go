package engine

import (
	"encoding/json"
	"errors"
	"sync"

	"github.com/thisisjun786/codex-relay-workflow/internal/executor"
)

type ExecutorLedger struct {
	mu      sync.Mutex
	journal Journal
	runID   string
	err     error
}

func NewExecutorLedger(j Journal, runID string) *ExecutorLedger {
	return &ExecutorLedger{journal: j, runID: runID}
}

func (l *ExecutorLedger) fail(err error) error {
	if l.err == nil && err != nil {
		l.err = err
	}
	return l.err
}

func (l *ExecutorLedger) journalErr() error {
	if fj, ok := l.journal.(FailingJournal); ok {
		return fj.Err()
	}
	return nil
}

func (l *ExecutorLedger) Transact(fn func([]executor.Record, func(executor.Record) error) error) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.journal == nil || l.runID == "" {
		return errors.New("executor ledger needs a journal and a run id")
	}
	if l.err != nil {
		return l.err
	}
	if err := journalFlush(l.journal); err != nil {
		return l.fail(err)
	}
	if err := l.journalErr(); err != nil {
		return l.fail(err)
	}
	var records []executor.Record
	for _, ev := range l.journal.Events() {
		if ev.RunID != l.runID || ev.Kind != KindFlow || ev.Op != "executor" || ev.Event != "executor_record" {
			continue
		}
		s, _ := ev.Data["record"].(string)
		var rec executor.Record
		if err := json.Unmarshal([]byte(s), &rec); err != nil {
			return l.fail(err)
		}
		records = append(records, rec)
	}
	return fn(records, func(rec executor.Record) error {
		raw, err := json.Marshal(rec)
		if err != nil {
			return err
		}
		l.journal.Append(Event{RunID: l.runID, Kind: KindFlow, Op: "executor", Event: "executor_record",
			Data: map[string]any{"record": string(raw)}})
		if err := journalFlush(l.journal); err != nil {
			return l.fail(err)
		}
		if err := l.journalErr(); err != nil {
			return l.fail(err)
		}
		return nil
	})
}
