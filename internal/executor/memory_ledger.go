package executor

import "sync"

type MemoryLedger struct {
	mu      sync.Mutex
	records []Record
}

func NewMemoryLedger() *MemoryLedger { return &MemoryLedger{} }

func (l *MemoryLedger) Transact(fn func(records []Record, appendRecord func(Record) error) error) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	var snap []Record
	for _, rec := range l.records {
		c, err := cloneRecord(rec)
		if err != nil {
			return err
		}
		snap = append(snap, c)
	}
	return fn(snap, func(r Record) error {
		c, err := cloneRecord(r)
		if err != nil {
			return err
		}
		l.records = append(l.records, c)
		return nil
	})
}
