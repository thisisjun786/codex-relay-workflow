package delivery

import (
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport"
)

// CRW-1071 (the follow-up CRW-945 left): the kept-acknowledgement confirmation and the recipient-turn check turned a
// store failure into a note or an outcome field and went on, so a failure of the corrupting class (CRW-848) never
// reached the daemon's halt branch. It now ends the pass and is returned, marked with the site that met it; every
// other failure stays the note it has been. The damage is real SQLITE_CORRUPT (code 11) on one table of a temporary
// store, so the rest of the store still answers.

// hostCheckWorld is a harness (no golden capture) the host-loss tests of this file run in.
func hostCheckWorld(t *testing.T) *hl {
	t.Helper()
	f := newFixture(t, "")
	f.rid = ""
	h := &hl{fixture: f, ack: NewAck(f.delivery), rc: NewReconciler(f.delivery), adapter: f.host, policy: defaultTick()}
	h.checks = &TurnChecks{Reconciler: h.rc, Budget: 4}
	return h
}

// keptAckWorld is a folded delivery whose acknowledgement the relay kept, because it could not yet confirm the
// send for that turn: ConfirmKeptAcks has one confirmation due.
func keptAckWorld(t *testing.T) (*hl, string) {
	t.Helper()
	h := hostCheckWorld(t)
	event, _ := h.foldedDelivery(false, 0, 0)
	h.cliAck(event, "folded", nil)
	if got := h.ackRow(event).Opt("last_reason"); got != DeliveryUnconfirmed {
		t.Fatalf("the acknowledgement was not kept for the unconfirmed delivery: %v", got)
	}
	h.clock.Advance(5)
	return h, event
}

// awaitingWorld is two delivered completions, the first turn finished and the second lost by the host.
func awaitingWorld(t *testing.T) *hl {
	t.Helper()
	h := hostCheckWorld(t)
	delivered := h.completions(2)
	h.host.finishTurn(parent, delivered[0][2], "completed")
	h.hostLoses(delivered[1][2], true)
	h.clock.Advance(120)
	return h
}

func TestConfirmKeptAcksHalt_aCorruptingFailureEndsThePassAndIsReturned(t *testing.T) {
	t.Parallel()
	for _, c := range []struct{ table, site string }{
		{"ack_evidence", store.HaltSiteObservation}, // the listing of the kept acknowledgements
		{"attempts", store.HaltSiteObservation},     // the confirmation's own read of the current attempt
	} {
		t.Run(c.table, func(t *testing.T) {
			h, _ := keptAckWorld(t)
			testsupport.DamageTable(t, h.store.DB, h.store.Path, c.table)
			_, err := confirmKept(h, h.clock.Now())
			requireSite(t, err, c.site)
		})
	}
}

func TestConfirmKeptAcksHalt_aReconciliationFailureOfTheConfirmationIsReturned(t *testing.T) {
	t.Parallel()
	h, event := keptAckWorld(t)
	testsupport.DamageTable(t, h.store.DB, h.store.Path, "journal")
	notes, err := confirmKept(h, h.clock.Now())
	requireCorruption(t, err)
	if len(notes) != 0 {
		t.Fatalf("the failure was also noted: %v", notes)
	}
	if got := h.ackRow(event).Opt("last_reason"); got != DeliveryUnconfirmed {
		t.Fatalf("the acknowledgement moved on a damaged store: %v", got)
	}
}

func TestConfirmKeptAcksHalt_aFailureThatIsNotCorruptionStaysANote(t *testing.T) {
	t.Parallel()
	h, event := keptAckWorld(t)
	if _, err := h.store.DB.Exec("DROP TABLE journal"); err != nil {
		t.Fatal(err)
	}
	notes, err := confirmKept(h, h.clock.Now())
	if err != nil {
		t.Fatalf("a failure that is not corruption ended the pass: %v", err)
	}
	if len(notes) != 1 || !strings.Contains(notes[0], "kept acknowledgement "+event+" not confirmed") {
		t.Fatalf("notes %v", notes)
	}
}

func TestConfirmDeliveryHalt_aCorruptingReconciliationFailureIsReturnedNotReported(t *testing.T) {
	t.Parallel()
	h, event := keptAckWorld(t)
	testsupport.DamageTable(t, h.store.DB, h.store.Path, "journal")
	outcome, err := h.rc.ConfirmDelivery(h.ctx, event, h.host, "folded")
	requireCorruption(t, err)
	if outcome != nil {
		t.Fatalf("the failure also came back as an outcome: %v", outcome)
	}
}

func TestTurnChecksHalt_aCorruptingListingFailureEndsThePassAndIsReturned(t *testing.T) {
	t.Parallel()
	h := awaitingWorld(t)
	testsupport.DamageTable(t, h.store.DB, h.store.Path, "deliveries")
	var report TurnCheckReport
	requireSite(t, passChecks(h, h.clock.Now(), &report), store.HaltSiteObservation)
	if len(report.Notes) != 0 || report.TurnsLost != 0 {
		t.Fatalf("the failure was also noted or counted: %+v", report)
	}
}

// The check of a lost turn writes the loss; the journal row it appends is the statement that meets the damage.
func TestTurnChecksHalt_aCorruptingCheckFailureEndsThePassAndIsReturned(t *testing.T) {
	t.Parallel()
	h := awaitingWorld(t)
	testsupport.DamageTable(t, h.store.DB, h.store.Path, "journal")
	var report TurnCheckReport
	requireSite(t, passChecks(h, h.clock.Now(), &report), store.HaltSiteWrite)
	if len(report.Notes) != 0 {
		t.Fatalf("the failure was also noted: %v", report.Notes)
	}
}

func TestTurnChecksHalt_aFailureThatIsNotCorruptionStaysANote(t *testing.T) {
	t.Parallel()
	h := awaitingWorld(t)
	if _, err := h.store.DB.Exec("DROP TABLE journal"); err != nil {
		t.Fatal(err)
	}
	var report TurnCheckReport
	if err := passChecks(h, h.clock.Now(), &report); err != nil {
		t.Fatalf("a failure that is not corruption ended the pass: %v", err)
	}
	if len(report.Notes) == 0 || !strings.Contains(strings.Join(report.Notes, "\n"), "recipient turn check failed for") {
		t.Fatalf("notes %v", report.Notes)
	}
}

// The two passes as the daemon calls them.
func confirmKept(h *hl, now float64) ([]string, error) {
	return ConfirmKeptAcks(h.ctx, h.ack, h.rc, h.adapter, now)
}

func passChecks(h *hl, now float64, report *TurnCheckReport) error {
	return h.checks.Pass(h.ctx, h.adapter, now, report)
}
