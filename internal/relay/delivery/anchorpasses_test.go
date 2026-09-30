package delivery

import "context"

// AnchorBinder is the repair pass a tick runs (Ack.BindPendingAnchors).
type AnchorBinder interface {
	BindPendingAnchors(ctx context.Context) ([]any, error)
}

type anchorBinderFunc func() ([]any, error)

func (f anchorBinderFunc) BindPendingAnchors(context.Context) ([]any, error) { return f() }

// AnchorReport is the binding half of the daemon's TickReport.
type AnchorReport struct {
	AnchorsBound int
	Passes       [][]any
	Notes        []string
}

// AnchorPasses is the binding contract of RelayDaemon.tick (todo 29 owns the rest of the tick):
// bind, reconcile, bind again, so a revision promoted by reconciliation binds in the same tick,
// and the two passes are ADDED, so the second cannot erase what the first repaired. A failed
// pass is a note, never the end of the tick.
func AnchorPasses(ctx context.Context, binder AnchorBinder, report *AnchorReport, reconcile func() error) error {
	pass := func() {
		bound, err := binder.BindPendingAnchors(ctx)
		if err != nil {
			report.Notes = append(report.Notes, "anchor recovery failed: "+err.Error())
			return
		}
		report.AnchorsBound += len(bound)
		report.Passes = append(report.Passes, bound)
	}
	pass()
	if err := reconcile(); err != nil {
		return err
	}
	pass()
	return nil
}
