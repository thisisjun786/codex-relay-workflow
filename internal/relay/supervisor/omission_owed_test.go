package supervisor

import (
	"fmt"
	"os"
	"reflect"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/delivery"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// CRW-300: DeriveOwedOmission answers what DeriveOmission answers for every omission the caller keeps, and
// answers "nothing to keep" for the rest, without reading the receipt. The grid below builds one
// relationship for each combination of the facts the derivation reads, over the omission world's
// receipted state, and compares the two for each of three clocks (a settlement long past, past with no
// grace, and inside the grace) and for both ways a caller names the turn (none, the newest admitted;
// and the dispatch's own turn, as omissionWithdrawn does).

type gridCase struct {
	declaration string // the declared outcome, or "none"
	settlement  string // none, completed, failed, interrupted or conflict
	receipt     string // final, staged, none or other (a final receipt of another turn)
	artifact    string // current, changed (a frozen copy still matches) or lost (changed, no copy)
	report      bool   // the daemon's own final report of the terminal
	later       bool   // a later turn admitted
}

func (c gridCase) String() string {
	return fmt.Sprintf("declaration=%s/settlement=%s/receipt=%s/artifact=%s/report=%v/later=%v", c.declaration, c.settlement, c.receipt, c.artifact, c.report, c.later)
}

func gridCases() []gridCase {
	var cases []gridCase
	for _, settlement := range []string{"none", "completed", "failed", "conflict"} {
		for _, later := range []bool{false, true} {
			for _, report := range []bool{false, true} {
				for _, receipt := range []string{"final", "staged", "none", "other"} {
					artifacts := []string{"current"}
					if receipt == "final" || receipt == "staged" {
						artifacts = []string{"current", "changed", "lost"}
					}
					for _, artifact := range artifacts {
						cases = append(cases, gridCase{"ready_for_review", settlement, receipt, artifact, report, later})
					}
				}
			}
		}
	}
	for _, declaration := range []string{"in_progress", "blocked_needs_input", "failed", "interrupted", "none"} {
		for _, settlement := range []string{"none", "completed", "failed", "interrupted"} {
			for _, later := range []bool{false, true} {
				cases = append(cases, gridCase{declaration, settlement, "final", "current", false, later})
			}
		}
	}
	return cases
}

// shape turns relationship i of the world, which starts in the receipted state, into c.
func (w *omissionWorld) shape(i int, c gridCase) {
	w.tb.Helper()
	rid := w.rels[i]
	child, turn, event := fmt.Sprintf("child-%04d", i), fmt.Sprintf("turn-%04d", i), fmt.Sprintf("ev-%04d-000", i)
	assignment := delivery.AssignmentID(fmt.Sprintf("dispatch-%04d", i))
	const settled = "2026-09-30T00:00:00.000000+00:00"
	if c.declaration == "none" {
		w.exec("DELETE FROM turn_declarations WHERE assignment_id=?", assignment)
	} else {
		w.exec("UPDATE turn_declarations SET outcome=? WHERE assignment_id=?", c.declaration, assignment)
	}
	w.exec("DELETE FROM assignment_settlements WHERE relationship_id=?", rid)
	terminals := map[string][]string{"none": nil, "completed": {"completed"}, "failed": {"failed"}, "interrupted": {"interrupted"}, "conflict": {"completed", "failed"}}[c.settlement]
	for _, terminal := range terminals {
		w.exec("INSERT INTO assignment_settlements (relationship_id,thread_id,turn_id,terminal_status,settled_at) VALUES (?,?,?,?,?)", rid, child, turn, terminal, settled)
	}
	switch c.receipt {
	case "staged":
		w.exec("UPDATE events SET stage='staged', staged_at=?, turn_status='inProgress' WHERE event_id=?", settled, event)
	case "none":
		w.exec("DELETE FROM work_reports WHERE event_id=?", event)
		w.exec("DELETE FROM events WHERE event_id=?", event)
	case "other":
		w.exec("UPDATE events SET turn_id='turn-of-another-time' WHERE event_id=?", event)
	}
	switch c.artifact {
	case "changed", "lost":
		if err := os.WriteFile(w.artifacts[i], []byte("the artifact changed after its receipt\n"), 0o600); err != nil {
			w.tb.Fatal(err)
		}
		if c.artifact == "lost" {
			w.exec("UPDATE events SET manifest_ref=NULL WHERE event_id=?", event)
		}
	}
	if c.report && len(terminals) > 0 {
		w.exec("INSERT INTO events (event_id,relationship_id,execution_generation,revision_hash,outcome,producer,turn_thread_id,turn_id,turn_status,receipt,stage,first_seen_at,last_seen_at) VALUES (?,?,1,'x',?,'daemon_observation',?,?,?,'{}','final',?,?)", fmt.Sprintf("ex-%04d", i), rid, terminals[0], child, turn, terminals[0], settled, settled)
	}
	if c.later {
		w.exec("INSERT INTO generation_turns (relationship_id,execution_generation,turn_id,evidence,actor,detail,admitted_at) VALUES (?,1,?,?,NULL,NULL,?)", rid, fmt.Sprintf("turn-later-%04d", i), fmt.Sprintf("explicit_admission_bound:turn-%04d", i), "2026-09-30T01:00:00.000000+00:00")
	}
}

func TestCRW300OwedOmissionAnswersAsTheWholeDerivationDoes(t *testing.T) {
	t.Parallel()
	cases := gridCases()
	w := seedOmissionWorld(t, len(cases), 0)
	for i, c := range cases {
		w.shape(i, c)
	}
	clocks := []struct {
		name  string
		now   string
		grace float64
	}{
		{"past", delivery.ISOOf(omissionNow), 300},
		{"no grace", delivery.ISOOf(omissionNow), 0},
		{"inside the grace", "2026-09-30T00:01:40.000000+00:00", 300},
	}
	var kept, skipped, judgedNotKept, skippedReads int
	for i, c := range cases {
		rid := w.rels[i]
		for _, clock := range clocks {
			for _, turn := range []string{"", fmt.Sprintf("turn-%04d", i)} {
				name := fmt.Sprintf("%s/%s/turn=%q", c, clock.name, turn)
				whole := delivery.DeriveOmission(w.ctx, w.s, store.PathlibParent(w.s.Path), rid, turn, clock.now, clock.grace)
				ctx, reads := store.WithArtifactReads(w.ctx)
				owed, judged := delivery.DeriveOwedOmission(ctx, w.s, store.PathlibParent(w.s.Path), rid, turn, clock.now, clock.grace)
				keep := objText(whole, "reportingState") == "unreported" && objBool(whole, "owed")
				switch {
				case judged:
					if !reflect.DeepEqual(owed, whole) {
						t.Errorf("%s: DeriveOwedOmission answered\n%v\nDeriveOmission answered\n%v", name, owed, whole)
					}
					if keep {
						kept++
					} else {
						judgedNotKept++
					}
				case keep:
					t.Errorf("%s: skipped an omission that DeriveOmission finds owed: %v", name, whole)
				default:
					skipped++
					skippedReads += int(reads.Count())
					if owed != nil {
						t.Errorf("%s: answered (%v, false)", name, owed)
					}
				}
			}
		}
	}
	if skippedReads != 0 {
		t.Errorf("the derivations that were skipped read %d artifacts", skippedReads)
	}
	t.Logf("%d relationships: %d kept as owed, %d judged and not kept, %d skipped", len(cases), kept, judgedNotKept, skipped)
	if kept == 0 || skipped == 0 || judgedNotKept == 0 {
		t.Errorf("the grid does not reach all three outcomes: kept %d, judged and not kept %d, skipped %d", kept, judgedNotKept, skipped)
	}
}
