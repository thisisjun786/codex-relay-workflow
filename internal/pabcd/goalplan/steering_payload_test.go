package goalplan

// The CRW-1111 cases of the steering transaction: the entry keeps the batch it applied (the annotate
// note and the dependency declarations included) and the rows it owes with stable ids; a retry with
// the same key and the same batch records a row the first attempt could not write and applies
// nothing again; the same key with another batch is refused; a legacy entry is left as it is. The
// cases marked red reproduce on dev (6a5851f4f).

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// steeringPayloadDepsBatch is the batch of the row-recovery cases: three work phases, two of which
// declare prerequisites, so the batch owes one steered row and two dependency_registered rows.
func steeringPayloadDepsBatch() map[string]any {
	return map[string]any{
		"idempotencyKey": "k-deps",
		"rationale":      "the parser splits in three",
		"evidence":       "devlog/_plan/x/091.md:3",
		"ops": []any{
			map[string]any{"kind": "add-work-phase", "id": "wp-a", "title": "lexer"},
			map[string]any{"kind": "add-work-phase", "id": "wp-b", "title": "parser", "dependsOn": []any{"wp-a"}},
			map[string]any{"kind": "add-work-phase", "id": "wp-c", "title": "printer", "dependsOn": []any{"wp-a", "wp-b"}},
		},
	}
}

// steeringPayloadCounts is the number of steered and dependency_registered rows.
func steeringPayloadCounts(t *testing.T, cwd, slug string) (int, int) {
	t.Helper()
	return steeringApplyEventRows(t, cwd, slug, EventSteered), steeringApplyEventRows(t, cwd, slug, EventDependencyRegistered)
}

// Red on dev: the note of an annotate that differs from the rationale and the evidence was validated
// and dropped, so no record of it survived the commit. It now stays in the entry, through a read and a
// later rewrite of the plan.
func TestSteeringAnnotateNoteSurvivesTheRoundTrip(t *testing.T) {
	cwd, slug := steeringApplyWorkspace(t)
	const note = "prefer streaming over a full buffer"
	steeringApply(t, cwd, slug, steeringApplyBatch(map[string]any{
		"ops": []any{map[string]any{"kind": "annotate", "note": note}},
	}), nil, SteerResultApplied)
	// A later batch rewrites the plan; the earlier entry's note must survive that rewrite.
	steeringApply(t, cwd, slug, steeringApplyBatch(map[string]any{"idempotencyKey": "k2",
		"ops": []any{map[string]any{"kind": "add-work-phase", "id": "wp-x", "title": "later"}}}), nil, SteerResultApplied)
	if text := steeringApplyPlanText(t, cwd, slug); !strings.Contains(text, note) {
		t.Fatalf("the annotate note is not in the stored plan:\n%s", text)
	}
}

// The recorded payload in typed form, and its events with stable ids.
func TestSteeringEntryRecordsTheBatchAndItsEvents(t *testing.T) {
	cwd, slug := steeringApplyWorkspace(t)
	steeringApply(t, cwd, slug, steeringPayloadDepsBatch(), nil, SteerResultApplied)
	stored := ReadGoalplan(cwd, slug)
	if stored == nil || len(stored.SteeringLog) != 1 {
		t.Fatalf("steeringLog = %#v", stored)
	}
	entry := stored.SteeringLog[0]
	if len(entry.Ops) != 3 || entry.Ops[2].ID != "wp-c" || strings.Join(entry.Ops[2].DependsOn, ",") != "wp-a,wp-b" || entry.Ops[0].DependsOn != nil {
		t.Errorf("ops = %#v", entry.Ops)
	}
	ids := []string{}
	for _, ev := range entry.Events {
		ids = append(ids, ev.ID+"="+string(ev.Event))
	}
	if got := strings.Join(ids, " "); got != `steer:"k-deps"=steered steer:"k-deps":op1=dependency_registered steer:"k-deps":op2=dependency_registered` {
		t.Errorf("events = %s", got)
	}
}

// Red on dev: an append that failed after the plan commit left the rows missing, and a retry with the
// same key answered duplicate at once, so they were lost for good. Now the retry records exactly the
// missing rows - one steered row and two dependency rows in all - and applies nothing again.
func TestSteeringRetryRecordsTheRowsAFailedAppendLost(t *testing.T) {
	cwd, slug := steeringApplyWorkspace(t)
	ledger := filepath.Join(steeringApplyDir(t, cwd, slug), GoalplanLedgerFile)
	if err := os.Mkdir(ledger, 0o777); err != nil {
		t.Fatal(err)
	}
	steeringApply(t, cwd, slug, steeringPayloadDepsBatch(), nil, SteerResultApplied)
	if err := os.Remove(ledger); err != nil {
		t.Fatal(err)
	}
	again := steeringApply(t, cwd, slug, steeringPayloadDepsBatch(), nil, SteerResultDuplicate)
	if again.Warning != "" {
		t.Errorf("the retry warned: %q", again.Warning)
	}
	if steered, deps := steeringPayloadCounts(t, cwd, slug); steered != 1 || deps != 2 {
		t.Fatalf("rows after the retry: steered %d, dependency_registered %d; want 1 and 2", steered, deps)
	}
	if stored := ReadGoalplan(cwd, slug); stored == nil || len(stored.WorkPhases) != 3 || len(stored.SteeringLog) != 1 {
		t.Fatalf("the retry applied the batch again: %#v", stored)
	}
}

// Each append of the batch fails in turn - the first, the second, the third - and the retry records the
// rest; a writer that stops right after the plan commit (the panic stands in for a kill) is the same.
func TestSteeringRetryRecordsTheRowsAfterEachFailedAppend(t *testing.T) {
	for failAt := 1; failAt <= 4; failAt++ {
		cwd, slug := steeringApplyWorkspace(t)
		calls := 0
		appendOnce := func(cwd, slug string, entry GoalplanLedgerEntry) error {
			calls++
			switch {
			case failAt == 4 && calls == 1:
				panic("stopped after the plan commit")
			case calls == failAt:
				return errors.New("injected append failure")
			}
			return AppendGoalplanLedger(cwd, slug, entry)
		}
		func() {
			defer func() { _ = recover() }()
			result, err := ApplySteeringBatch(cwd, slug, steeringPayloadDepsBatch(), &SteeringBatchOptions{appendLedger: appendOnce})
			if err != nil || result.Kind != SteerResultApplied || !strings.Contains(result.Warning, "Re-running the same batch with the same key records the missing rows") {
				t.Errorf("failAt %d: first attempt (%+v, %v)", failAt, result, err)
			}
		}()
		steeringApply(t, cwd, slug, steeringPayloadDepsBatch(), nil, SteerResultDuplicate)
		steeringApply(t, cwd, slug, steeringPayloadDepsBatch(), nil, SteerResultDuplicate)
		if steered, deps := steeringPayloadCounts(t, cwd, slug); steered != 1 || deps != 2 {
			t.Errorf("failAt %d: rows after the retries: steered %d, dependency_registered %d; want 1 and 2", failAt, steered, deps)
		}
		rows := steeringApplyRows(t, cwd, slug)
		if len(rows) < 3 || rows[len(rows)-3].Event != EventSteered {
			t.Errorf("failAt %d: the rows are out of the oracle's order: %+v", failAt, rows)
		}
		if stored := ReadGoalplan(cwd, slug); stored == nil || len(stored.WorkPhases) != 3 {
			t.Errorf("failAt %d: the retry applied the batch again: %#v", failAt, stored)
		}
	}
}

// Red on dev: the same key with another batch answered duplicate, so the other batch was reported done
// without being applied. It is now refused and writes nothing.
func TestSteeringSameKeyWithAnotherBatchIsRefused(t *testing.T) {
	cwd, slug := steeringApplyWorkspace(t)
	steeringApply(t, cwd, slug, steeringApplyBatch(nil), nil, SteerResultApplied)
	plan, ledger := steeringApplyPlanText(t, cwd, slug), steeringApplyLedgerText(t, cwd, slug)
	other := steeringApply(t, cwd, slug, steeringApplyBatch(map[string]any{
		"ops": []any{map[string]any{"kind": "annotate", "note": "a different note"}},
	}), nil, SteerResultRejected)
	if !strings.Contains(other.Reason, `idempotencyKey "k1" was already used`) {
		t.Errorf("reason = %q", other.Reason)
	}
	if steeringApplyPlanText(t, cwd, slug) != plan || steeringApplyLedgerText(t, cwd, slug) != ledger {
		t.Error("the refused batch wrote something")
	}
}

// A legacy entry (written before the payload was recorded) is answered duplicate, and nothing it may
// have lost is guessed at: no row is written for it.
func TestSteeringLegacyEntryIsLeftAsItIs(t *testing.T) {
	cwd, slug := steeringApplyWorkspace(t)
	locked, err := WithGoalplanWriteLock(cwd, slug, func(plan *Goalplan) (struct{}, error) {
		next := *plan
		next.SteeringLog = []SteeringEntry{{IdempotencyKey: "k1", Rationale: "old", Evidence: "old", AppliedAt: "2026-01-01T00:00:00.000Z", Summary: "1 op(s): annotate"}}
		return struct{}{}, WriteGoalplan(cwd, &next)
	}, nil)
	if err != nil || locked.Kind != "ok" {
		t.Fatalf("seed the legacy entry: %+v %v", locked, err)
	}
	ledger := steeringApplyLedgerText(t, cwd, slug)
	steeringApply(t, cwd, slug, steeringApplyBatch(nil), nil, SteerResultDuplicate)
	if steeringApplyLedgerText(t, cwd, slug) != ledger {
		t.Error("a legacy entry's retry wrote a row")
	}
}

// Two batches with the same clock whose steered rows spell the same detail (the key and the rationale trade the text between
// them) have different stable ids. The retry of the second, whose own append failed, records its row: the first batch's row is
// not taken for it.
func TestSteeringStableIDsActuallyDeduplicate(t *testing.T) {
	cwd, slug := steeringApplyWorkspace(t)
	now := func() string { return "2026-10-10T00:00:00.000Z" }
	first := map[string]any{"idempotencyKey": "k", "rationale": "r: 1 op(s): annotate — x", "evidence": "e",
		"ops": []any{map[string]any{"kind": "annotate", "note": "n"}}}
	second := map[string]any{"idempotencyKey": "k: 1 op(s): annotate — r", "rationale": "x", "evidence": "e",
		"ops": []any{map[string]any{"kind": "annotate", "note": "n"}}}
	if r, err := ApplySteeringBatch(cwd, slug, first, &SteeringBatchOptions{Now: now}); err != nil || r.Kind != SteerResultApplied || r.Warning != "" {
		t.Fatalf("first: %+v %v", r, err)
	}
	failing := func(string, string, GoalplanLedgerEntry) error { return errors.New("injected append failure") }
	if r, err := ApplySteeringBatch(cwd, slug, second, &SteeringBatchOptions{Now: now, appendLedger: failing}); err != nil || r.Kind != SteerResultApplied || r.Warning == "" {
		t.Fatalf("second: %+v %v", r, err)
	}
	if r, err := ApplySteeringBatch(cwd, slug, second, &SteeringBatchOptions{Now: now}); err != nil || r.Kind != SteerResultDuplicate || r.Warning != "" {
		t.Fatalf("second retry: %+v %v", r, err)
	}
	rows := steeringApplyRows(t, cwd, slug)
	steered := 0
	for _, row := range rows {
		if row.Event == EventSteered {
			steered++
			if row.EventID == nil {
				t.Errorf("a steered row without its event id: %+v", row)
			}
		}
	}
	if steered != 2 {
		t.Fatalf("steered rows = %d, want 2: %+v", steered, rows)
	}
	// The same retry again adds nothing.
	steeringApply(t, cwd, slug, second, nil, SteerResultDuplicate)
	if got := steeringApplyEventRows(t, cwd, slug, EventSteered); got != 2 {
		t.Fatalf("steered rows after another retry = %d", got)
	}
}

// A batch whose key is the id suffix of another batch's dependency row does not collide with it.
func TestSteeringEventIDsOfDifferentKeysNeverCollide(t *testing.T) {
	a := steeringEvents("k", "s", "r", []SteerOp{{Kind: SteerOpAddWorkPhase, ID: "wp", DependsOn: []string{"x"}}})
	b := steeringEvents("k:op0", "s", "r", nil)
	if a[1].ID == b[0].ID {
		t.Fatalf("ids collide: %q", a[1].ID)
	}
}
