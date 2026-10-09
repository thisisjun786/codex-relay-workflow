package daemon

import (
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport"
)

// CRW-945 and CRW-1004, evaluation of 901ee68a: the requeue and the sweep's recording keep the site the statement
// that met the damage marked. A read inside them (the event row Enqueue reads before it writes, the ledger row
// Record reads before it writes) is the observation site; only a statement that changes the store is the write
// site. The outer step must not replace the inner mark.

// A due intent survives the requeue scan and Enqueue's read of its event row meets the damage: the error the
// requeue returns says observation.
func TestHaltRound3_aRequeueReadOfTheEventRowIsMarkedAtTheObservationSite(t *testing.T) {
	t.Parallel()
	d, s, ctx := haltCoverageDaemon(t, &observationHost{status: "completed"})
	ownReceipt(t, s, "business", "ready_for_review", "final", "", owedIntent)
	exec(t, s, "UPDATE delivery_intent SET kind='completion_event', next_retry_at=NULL")
	testsupport.DamageTable(t, s.DB, s.Path, "events")
	err := d.requeue(ctx, &Report{}, 1700000000)
	if _, ok := store.CorruptingFailure(err); !ok {
		t.Fatalf("the fixture did not return a corrupt read: %v", err)
	}
	if site := store.SiteOf(err, ""); site != store.HaltSiteObservation {
		t.Fatalf("Enqueue's read of the event row was marked %q, not the observation site", site)
	}
}
