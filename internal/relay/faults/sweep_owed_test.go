package faults

import (
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/delivery"
)

// crw263Reading is the reading the relay's own classifier gives a turn that settled without a
// report, in the shape a reader hands the sweep. The classifier's answer is an ordered object that
// is not a JSON object when marshalled by encoding/json, so it goes through the same dumps and loads
// the CLI ingress uses, which gives the bool owed a decoded reading carries.
func crw263Reading(facts delivery.OmissionFacts, relationship string) map[string]any {
	witnessed := true
	facts.Witness = &witnessed
	facts.Admission = "admitted"
	facts.Settlements = []delivery.OmissionSettlement{{Status: "interrupted", At: "1970-01-02T03:46:40+00:00"}}
	facts.Label = "undeclared_turn_end"
	reading := loadsMap(dumps(delivery.ClassifyOmission(facts), true))
	reading["schema"] = "reporting-observation/1"
	reading["relationshipId"] = relationship
	reading["selectors"] = map[string]any{"turn": "turn"}
	return reading
}

// An unreported reading that says nothing is owed for it is not an omission. A later admitted turn
// (the daemon does not report an earlier turn's end once a later turn holds a final receipt), or a
// receipt of the turn itself, reads owed false, and the supervisor's readers already skip such a
// reading; the sweep filed it as report_omitted all the same. The classifier decides what is owed,
// so the readings below come from it with the fact set, not from a store holding a receipt. A
// genuine omission, and a reading that carries no owed field (one handed in from outside), are filed
// as they always were.
func TestCRW263SweepFilesAnUnreportedTurnOnlyWhenItIsOwed(t *testing.T) {
	for _, c := range []struct {
		name       string
		reading    func() map[string]any
		owed       any // what the reading says; nil when it carries no owed field
		owedReason any
	}{
		{"a later turn is admitted", func() map[string]any {
			return crw263Reading(delivery.OmissionFacts{LaterAdmitted: true}, "rel")
		}, false, delivery.OmittedLaterTurn},
		{"the turn's own receipt exists", func() map[string]any {
			return crw263Reading(delivery.OmissionFacts{Receipted: true}, "rel")
		}, false, delivery.OmittedReceipted},
		{"control: a genuine omission", func() map[string]any {
			return crw263Reading(delivery.OmissionFacts{}, "rel")
		}, true, delivery.OmittedReason},
		{"control: a reading with no owed field", func() map[string]any {
			reading := crw263Reading(delivery.OmissionFacts{}, "rel")
			delete(reading, "owed")
			delete(reading, "owedReason")
			return reading
		}, nil, nil},
	} {
		t.Run(c.name, func(t *testing.T) {
			ctx, gd := f1ReplayStores(t)
			reading := c.reading()
			if reading["reportingState"] != "unreported" || reading["owed"] != c.owed || reading["owedReason"] != c.owedReason {
				t.Fatalf("the classifier answered %v, want unreported with owed %v (%v)", reading, c.owed, c.owedReason)
			}
			s := fcOpen(t, ctx, gd)
			l := &Ledger{Store: s, Clock: &testClock{now: 100000}}
			sw := &Sweeper{Store: s, Now: l.Clock.ISO, HostRecordPath: testHostRecordPath(), Installation: Installation{Package: "codex-session-relay", Version: RelayPackageVersion, Location: relayPackageLocation}}
			batch, err := sw.SweepReadings(ctx, "crw", "", []any{reading}, 0)
			if err != nil {
				t.Fatal(err)
			}
			if len(batch.Gaps) != 0 {
				t.Fatalf("the sweep did not accept the reading: %v", batch.Gaps)
			}
			filed := []Observation{}
			for _, o := range batch.Observations {
				if o.FaultClass == "report_omitted" && !o.Cleared {
					filed = append(filed, o)
				}
			}
			if _, err = sw.RecordAll(ctx, l, batch); err != nil {
				t.Fatal(err)
			}
			rows, err := s.All(ctx, "SELECT severity,signature FROM fault_ledger WHERE fault_class='report_omitted'")
			if err != nil {
				t.Fatal(err)
			}
			if c.owed == false {
				if len(filed) != 0 || len(rows) != 0 {
					t.Fatalf("a turn that is not owed was recorded as report_omitted: %d observations, ledger rows %v", len(filed), rows)
				}
				return
			}
			if len(filed) != 1 || filed[0].Severity != Broken || filed[0].OccurrenceKey != "observation:rel:turn" || len(rows) != 1 || rows[0].Text("severity") != Broken {
				t.Fatalf("a genuine omission was not recorded as report_omitted (broken): %d observations, ledger rows %v", len(filed), rows)
			}
		})
	}
}
