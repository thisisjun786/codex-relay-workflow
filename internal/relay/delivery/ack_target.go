package delivery

import (
	"context"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// AckTargetNotice is the comparison of the integration target with the baselineRevision of the child's intent (CRW-1302), installed by
// the scheduler package when it is linked (the delivery package cannot import the fault ledger the comparison records into). It reads
// only the store and the child's intent file: no network call and no git call. It answers the notice the acknowledgement carries, or nil.
// Nil leaves every acknowledgement as it was.
var AckTargetNotice TargetNoticeFunc

// TargetNoticeFunc is the shape of that comparison: it reads the store and the intent file, and answers the notice or nil.
type TargetNoticeFunc func(ctx context.Context, s *store.Store, event string) (Obj, error)

// withTargetNotice adds the notice to an acknowledgement's record as targetMoved. The comparison warns and records and the step
// proceeds: an error of the comparison, like a comparison that finds nothing, leaves the record as it is.
func withTargetNotice(ctx context.Context, a *Ack, event string, record Obj) Obj {
	if a == nil || a.TargetNotice == nil || a.Store == nil {
		return record
	}
	notice, err := a.TargetNotice(ctx, a.Store, event)
	if err != nil || notice == nil {
		return record
	}
	return append(record, F{Key: "targetMoved", Value: notice})
}
