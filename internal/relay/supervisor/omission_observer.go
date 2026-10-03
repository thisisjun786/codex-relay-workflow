package supervisor

import (
	"context"
	"errors"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/delivery"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/faults"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// omissionObserverGrace is the report grace the fault sweep judges with: none. A settled turn that owes a
// report is filed as soon as the sweep reads it, as the sweep has always done. The supervisor's standing
// readers wait 300 seconds before they call a turn owed; applying that wait here would delay faults and is
// a separate decision (docs/port/refactor-backlog.md).
const omissionObserverGrace = 0.0

// OmissionObserver is the faults.ManagedReadingObserver the relay daemon's fault sweep reads its settled
// managed turns through. It answers with delivery's own omission judgment (delivery.ObserveOmission, which
// classifies with delivery.ClassifyOmission), so a reading says whether a report is still owed: a turn that
// a later admitted turn or its own final receipt has overtaken reads unreported with owed false, and the
// sweep files nothing for it. The package faults cannot call delivery (delivery's tests import faults), so
// the daemon installs this observer, in adapter.daemonFactory, in the field faults.Sweeper.ManagedObserver.
type OmissionObserver struct{}

var _ faults.ManagedReadingObserver = OmissionObserver{}

// Observe reads the exact settled turn r names, without writing the marker tree or the store. The
// selection must be the store.StateSelection the sweep was given.
func (OmissionObserver) Observe(ctx context.Context, r faults.ManagedReadingRequest) (any, error) {
	selection, ok := r.Selection.(store.StateSelection)
	if !ok {
		return nil, errors.New("TypeError: managed readings need a store selection")
	}
	reading := delivery.ObserveOmission(ctx, selection, r.Root, r.Workspace, r.Assignment, r.Session, r.Turn, r.Now, omissionObserverGrace)
	return orderedMap(reading), nil
}
