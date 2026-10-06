package goalplan

// The transaction cases of CXC v0.2.40 test/steering.test.ts (commit 3c1459ac) that need
// applySteeringBatch: the idempotency key, the shared goalplan write lock, the ledger rows and
// the answers applied, duplicate, locked and rejected. The validation and the pure fold stay in
// steering_ops_test.go, which owns the batch shape and the op grammar this file drives.
import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// steeringApplyWorkspace is the oracle's workspace() (:46-50): a fresh temporary cwd holding one
// built plan. The slug is the built plan's own, so every case addresses the same plan.
func steeringApplyWorkspace(t *testing.T) (string, string) {
	t.Helper()
	cwd := t.TempDir()
	plan := BuildGoalplan(NewGoalplanInput{Objective: "steering fixture"})
	if err := WriteGoalplan(cwd, plan); err != nil {
		t.Fatalf("seed plan: %v", err)
	}
	return cwd, plan.Slug
}

// steeringApplyBatch is steering.test.ts's batch(): one annotate op, overridable.
func steeringApplyBatch(over map[string]any) map[string]any {
	batch := map[string]any{
		"idempotencyKey": "k1",
		"rationale":      "the scope shifted after the audit",
		"evidence":       "devlog/_plan/x/090.md:12",
		"ops":            []any{map[string]any{"kind": "annotate", "note": "narrowed to the parser"}},
	}
	for key, value := range over {
		batch[key] = value
	}
	return batch
}

// steeringApplyDir is the plan's directory, where the lock and the ledger live.
func steeringApplyDir(t *testing.T, cwd, slug string) string {
	t.Helper()
	dir, err := GoalplanDir(cwd, slug)
	if err != nil {
		t.Fatalf("goalplan dir: %v", err)
	}
	return dir
}

// steeringApplyLedgerText is the oracle's ledgerText(): the ledger's bytes, empty when absent.
func steeringApplyLedgerText(t *testing.T, cwd, slug string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(steeringApplyDir(t, cwd, slug), GoalplanLedgerFile))
	if err != nil {
		return ""
	}
	return string(raw)
}

// steeringApplyPlanText is the plan file's bytes, which the oracle compares before and after.
func steeringApplyPlanText(t *testing.T, cwd, slug string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(steeringApplyDir(t, cwd, slug), GoalplanFile))
	if err != nil {
		t.Fatalf("read plan: %v", err)
	}
	return string(raw)
}

// steeringApplyRows is the ledger's rows, parsed: the oracle reads an event by name rather than
// comparing a whole line.
func steeringApplyRows(t *testing.T, cwd, slug string) []GoalplanLedgerEntry {
	t.Helper()
	rows := []GoalplanLedgerEntry{}
	text := strings.TrimSuffix(steeringApplyLedgerText(t, cwd, slug), "\n")
	for _, line := range strings.Split(text, "\n") {
		if line == "" {
			continue
		}
		var entry GoalplanLedgerEntry
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			t.Fatalf("ledger row %q: %v", line, err)
		}
		rows = append(rows, entry)
	}
	return rows
}

// steeringApplyEventRows counts the rows of one event.
func steeringApplyEventRows(t *testing.T, cwd, slug string, event GoalplanLedgerEvent) int {
	t.Helper()
	count := 0
	for _, row := range steeringApplyRows(t, cwd, slug) {
		if row.Event == event {
			count++
		}
	}
	return count
}

// steeringApply runs one batch and asserts the answer's kind, returning it. A Go error fails the
// case: the oracle's throw path is reached only through an unusable slug or a failed write, and
// no case here drives one.
func steeringApply(t *testing.T, cwd, slug string, batch any, o *SteeringBatchOptions, want SteerResultKind) SteerResult {
	t.Helper()
	result, err := ApplySteeringBatch(cwd, slug, batch, o)
	if err != nil {
		t.Fatalf("ApplySteeringBatch: %v", err)
	}
	if result.Kind != want {
		t.Fatalf("kind = %q (reason %q), want %q", result.Kind, result.Reason, want)
	}
	return result
}

// "a valid batch applies once, records the entry and one ledger line" (:62-72).
func TestSteeringApplyRecordsTheEntryAndOneLedgerLine(t *testing.T) {
	cwd, slug := steeringApplyWorkspace(t)
	result := steeringApply(t, cwd, slug, steeringApplyBatch(nil),
		&SteeringBatchOptions{Now: func() string { return "2026-03-03T00:00:00.000Z" }}, SteerResultApplied)
	if result.Entry == nil {
		t.Fatal("applied without an entry")
	}
	if result.Entry.IdempotencyKey != "k1" {
		t.Errorf("idempotencyKey = %q", result.Entry.IdempotencyKey)
	}
	if result.Entry.AppliedAt != "2026-03-03T00:00:00.000Z" {
		t.Errorf("appliedAt = %q", result.Entry.AppliedAt)
	}
	if result.Entry.Summary != "1 op(s): annotate" {
		t.Errorf("summary = %q", result.Entry.Summary)
	}
	stored := ReadGoalplan(cwd, slug)
	if stored == nil || len(stored.SteeringLog) != 1 {
		t.Fatalf("steeringLog = %#v", stored)
	}
	if got := steeringApplyEventRows(t, cwd, slug, EventSteered); got != 1 {
		t.Errorf("steered rows = %d, want 1", got)
	}
}

// "re-running the same key is a no-op that writes nothing" (:74-82): the duplicate answers the
// STORED entry, so a batch whose words differ under the same key changes nothing.
func TestSteeringApplyDuplicateKeyIsANoOp(t *testing.T) {
	cwd, slug := steeringApplyWorkspace(t)
	steeringApply(t, cwd, slug, steeringApplyBatch(nil), nil, SteerResultApplied)
	before := steeringApplyLedgerText(t, cwd, slug)

	again := steeringApply(t, cwd, slug,
		steeringApplyBatch(map[string]any{"rationale": "different words, same key"}), nil, SteerResultDuplicate)
	if again.Entry == nil {
		t.Fatal("duplicate without an entry")
	}
	if again.Entry.Rationale != "the scope shifted after the audit" {
		t.Errorf("duplicate entry rationale = %q, want the stored one", again.Entry.Rationale)
	}
	if stored := ReadGoalplan(cwd, slug); stored == nil || len(stored.SteeringLog) != 1 {
		t.Fatalf("steeringLog = %#v", stored)
	}
	if ledger := steeringApplyLedgerText(t, cwd, slug); ledger != before {
		t.Errorf("the duplicate wrote a ledger line: %q", ledger)
	}
}

// The batch is validated BEFORE the lock is taken (:259-260 precede :264), so a batch the
// validator refuses is answered with its own reason even when nothing exists at the slug, and no
// state is created.
func TestSteeringApplyValidationPrecedesTheLock(t *testing.T) {
	cwd := t.TempDir()
	result := steeringApply(t, cwd, "no-such-plan",
		steeringApplyBatch(map[string]any{"rationale": ""}), nil, SteerResultRejected)
	if result.Reason != "rationale is required and must be a non-empty string" {
		t.Errorf("reason = %q", result.Reason)
	}
	if _, err := os.Stat(filepath.Join(cwd, ".crw")); !os.IsNotExist(err) {
		t.Error("a refused batch created state")
	}
}

// "a held common lock blocks the batch and preserves plan and ledger bytes" (:172-193). The
// pre-created lock directory is the filesystem state "another writer holds it"; the injected
// empty retry list means no sleep is configured, so a retry would be a bug.
func TestSteeringApplyHeldLockBlocksAndPreservesBytes(t *testing.T) {
	cwd, slug := steeringApplyWorkspace(t)
	lock := filepath.Join(steeringApplyDir(t, cwd, slug), GoalplanLockDir)
	if err := os.Mkdir(lock, 0o777); err != nil {
		t.Fatalf("hold the lock: %v", err)
	}
	owner, err := json.Marshal(map[string]any{"pid": 4242, "acquiredAt": "2026-08-29T00:00:00.000Z"})
	if err != nil {
		t.Fatalf("encode owner.json: %v", err)
	}
	if err := os.WriteFile(filepath.Join(lock, GoalplanLockOwnerFile), owner, 0o600); err != nil {
		t.Fatalf("write owner.json: %v", err)
	}
	beforePlan, beforeLedger := steeringApplyPlanText(t, cwd, slug), steeringApplyLedgerText(t, cwd, slug)

	result := steeringApply(t, cwd, slug, steeringApplyBatch(nil),
		&SteeringBatchOptions{Lock: &GoalplanWriteLockOptions{RetryDelaysMs: []int{}, Sleep: func(int) {
			t.Error("no sleep is configured")
		}}}, SteerResultLocked)
	if !strings.Contains(result.Reason, "4242") {
		t.Errorf("reason does not name the owner pid: %q", result.Reason)
	}
	if !strings.Contains(result.Reason, GoalplanLockDir) {
		t.Errorf("reason does not name the lock directory: %q", result.Reason)
	}
	if steeringApplyPlanText(t, cwd, slug) != beforePlan {
		t.Error("the held lock changed the plan")
	}
	if steeringApplyLedgerText(t, cwd, slug) != beforeLedger {
		t.Error("the held lock changed the ledger")
	}
}

// "one invalid op rejects the whole batch" (:95-111) and the fold's own refusal (:143-163): a
// rejected batch writes neither the plan nor the ledger, whether the validator or applyOps
// refused it.
func TestSteeringApplyRejectedWritesNothing(t *testing.T) {
	for _, test := range []struct {
		name  string
		batch map[string]any
		want  string
	}{
		{
			name: "the validator refuses an op",
			batch: steeringApplyBatch(map[string]any{"ops": []any{
				map[string]any{"kind": "annotate", "note": "fine"},
				map[string]any{"kind": "annotate", "note": "also fine"},
				map[string]any{"kind": "annotate"},
			}}),
			want: "ops[2] is an annotate without a note",
		},
		{
			name: "applyOps refuses a duplicate work-phase id",
			batch: steeringApplyBatch(map[string]any{
				"idempotencyKey": "k-add-wp-2",
				"ops": []any{map[string]any{
					"kind": "add-work-phase", "id": "wp99-new", "title": "Duplicate",
				}},
			}),
			want: "work phase 'wp99-new' is already in this plan",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			cwd, slug := steeringApplyWorkspace(t)
			if test.name == "applyOps refuses a duplicate work-phase id" {
				steeringApply(t, cwd, slug, steeringApplyBatch(map[string]any{
					"idempotencyKey": "k-add-wp",
					"ops": []any{map[string]any{
						"kind": "add-work-phase", "id": "wp99-new", "title": "Newly scoped work",
					}},
				}), nil, SteerResultApplied)
			}
			beforePlan, beforeLedger := steeringApplyPlanText(t, cwd, slug), steeringApplyLedgerText(t, cwd, slug)
			result := steeringApply(t, cwd, slug, test.batch, nil, SteerResultRejected)
			if result.Reason != test.want {
				t.Errorf("reason = %q, want %q", result.Reason, test.want)
			}
			if steeringApplyPlanText(t, cwd, slug) != beforePlan {
				t.Error("a rejected batch changed the plan")
			}
			if steeringApplyLedgerText(t, cwd, slug) != beforeLedger {
				t.Error("a rejected batch changed the ledger")
			}
		})
	}
}

// "a failed ledger append still succeeds, with a warning and the entry intact" (:212-223). A
// directory at the ledger path makes the append fail, and it lands after the goalplan commit;
// removing write permission instead would break the plan write first and never reach this path.
func TestSteeringApplyFailedLedgerAppendStillAppliesWithWarning(t *testing.T) {
	cwd, slug := steeringApplyWorkspace(t)
	if err := os.Mkdir(filepath.Join(steeringApplyDir(t, cwd, slug), GoalplanLedgerFile), 0o777); err != nil {
		t.Fatalf("block the ledger path: %v", err)
	}
	result := steeringApply(t, cwd, slug, steeringApplyBatch(nil), nil, SteerResultApplied)
	if result.Warning == "" {
		t.Fatal("a missing audit line was not reported")
	}
	want := "the batch was applied but its ledger entry could not be written to .crw/goalplans/" +
		slug + "/" + GoalplanLedgerFile
	if !strings.Contains(result.Warning, want) {
		t.Errorf("warning = %q, want it to name %q", result.Warning, want)
	}
	if !strings.HasSuffix(result.Warning, "Re-running is a no-op because the key is recorded.") {
		t.Errorf("warning = %q", result.Warning)
	}
	if stored := ReadGoalplan(cwd, slug); stored == nil || len(stored.SteeringLog) != 1 {
		t.Fatalf("the commit did not stand: %#v", stored)
	}
}

// "an unbound slug is refused before anything is touched" (:225-230): the shared lock reports the
// absent plan among its unreadable reasons, and the answer is mapped back to the pre-lock wording.
func TestSteeringApplyUnboundSlugIsRefused(t *testing.T) {
	cwd := t.TempDir()
	result := steeringApply(t, cwd, "no-such-plan", steeringApplyBatch(nil), nil, SteerResultRejected)
	if result.Reason != "no goalplan found at slug 'no-such-plan'" {
		t.Errorf("reason = %q", result.Reason)
	}
	if _, err := os.Stat(filepath.Join(cwd, ".crw")); !os.IsNotExist(err) {
		t.Error("the refusal created state")
	}
}

// "the ledger entry stays compact — no copy of the plan" (:232-239).
func TestSteeringApplyLedgerRowStaysCompact(t *testing.T) {
	cwd, slug := steeringApplyWorkspace(t)
	steeringApply(t, cwd, slug, steeringApplyBatch(nil), nil, SteerResultApplied)
	line := strings.TrimSpace(steeringApplyLedgerText(t, cwd, slug))
	if len(line) >= 400 {
		t.Errorf("ledger line is %d chars", len(line))
	}
	if strings.Contains(line, "workPhases") || strings.Contains(line, "criteria") {
		t.Errorf("the ledger line copies the plan: %q", line)
	}
}

// "steeringLog survives a write/read round trip" (:241-247).
func TestSteeringApplyLogSurvivesARoundTrip(t *testing.T) {
	cwd, slug := steeringApplyWorkspace(t)
	steeringApply(t, cwd, slug, steeringApplyBatch(nil), nil, SteerResultApplied)
	back := ReadGoalplan(cwd, slug)
	if back == nil || len(back.SteeringLog) != 1 {
		t.Fatalf("steeringLog = %#v", back)
	}
	if back.SteeringLog[0].IdempotencyKey != "k1" || back.SteeringLog[0].Evidence != "devlog/_plan/x/090.md:12" {
		t.Errorf("entry = %#v", back.SteeringLog[0])
	}
}

// "add-work-phase stores dependencies and records one success event" (:324-342): the stored phase
// keeps its dependsOn, and the ledger gains exactly one dependency_registered row, below the
// steered row that carried the batch.
func TestSteeringApplyDependencyRegisteredRow(t *testing.T) {
	cwd, slug := steeringApplyWorkspace(t)
	seeded := ReadGoalplan(cwd, slug)
	if seeded == nil {
		t.Fatal("the seeded plan did not read back")
	}
	seeded.WorkPhases = []GoalplanWorkPhase{
		{ID: "wp-a", Title: "A", Status: WorkPhaseDone, Tasks: []GoalplanTask{}, CriteriaIDs: []string{}},
		{ID: "wp-b", Title: "B", Status: WorkPhaseDone, Tasks: []GoalplanTask{}, CriteriaIDs: []string{}},
	}
	if err := WriteGoalplan(cwd, seeded); err != nil {
		t.Fatalf("seed the phases: %v", err)
	}
	result := steeringApply(t, cwd, slug, steeringApplyBatch(map[string]any{
		"idempotencyKey": "k-add-wp-deps",
		"ops": []any{map[string]any{
			"kind": "add-work-phase", "id": "wp-c", "title": "C", "dependsOn": []any{"wp-a", "wp-b"},
		}},
	}), nil, SteerResultApplied)
	if result.Entry == nil || result.Entry.Summary != "1 op(s): add-work-phase" {
		t.Fatalf("entry = %#v", result.Entry)
	}
	stored := ReadGoalplan(cwd, slug)
	if stored == nil || len(stored.WorkPhases) != 3 {
		t.Fatalf("workPhases = %#v", stored)
	}
	added := stored.WorkPhases[2]
	if added.ID != "wp-c" || added.Status != WorkPhasePending {
		t.Errorf("added phase = %#v", added)
	}
	if len(added.DependsOn) != 2 || added.DependsOn[0] != "wp-a" || added.DependsOn[1] != "wp-b" {
		t.Errorf("dependsOn = %#v", added.DependsOn)
	}
	rows := steeringApplyRows(t, cwd, slug)
	if len(rows) != 2 {
		t.Fatalf("ledger rows = %d, want 2: %#v", len(rows), rows)
	}
	if rows[0].Event != EventSteered {
		t.Errorf("first row = %#v, want steered", rows[0])
	}
	if rows[1].Event != EventDependencyRegistered {
		t.Errorf("second row = %#v, want dependency_registered", rows[1])
	}
	if rows[1].Detail != "wp-c dependsOn=wp-a,wp-b" {
		t.Errorf("dependency row detail = %q", rows[1].Detail)
	}
}

// The steered row is the oracle's byte for byte (:286-291): "<key>: <summary> — <rationale>",
// with an em dash and one space either side.
func TestSteeringApplySteeredRowDetail(t *testing.T) {
	cwd, slug := steeringApplyWorkspace(t)
	steeringApply(t, cwd, slug, steeringApplyBatch(nil),
		&SteeringBatchOptions{Now: func() string { return "2026-03-03T00:00:00.000Z" }}, SteerResultApplied)
	rows := steeringApplyRows(t, cwd, slug)
	if len(rows) != 1 {
		t.Fatalf("rows = %#v", rows)
	}
	if rows[0].Ts != "2026-03-03T00:00:00.000Z" || rows[0].Slug != slug {
		t.Errorf("row = %#v", rows[0])
	}
	if want := "k1: 1 op(s): annotate — the scope shifted after the audit"; rows[0].Detail != want {
		t.Errorf("detail = %q, want %q", rows[0].Detail, want)
	}
}

// "the common lock is released after an applied or rejected batch" (:195-210).
func TestSteeringApplyLockReleasedAfterAppliedAndRejected(t *testing.T) {
	cwd, slug := steeringApplyWorkspace(t)
	lock := filepath.Join(steeringApplyDir(t, cwd, slug), GoalplanLockDir)

	steeringApply(t, cwd, slug, steeringApplyBatch(nil), nil, SteerResultApplied)
	if _, err := os.Stat(lock); !os.IsNotExist(err) {
		t.Errorf("the lock survived an applied batch: %v", err)
	}
	steeringApply(t, cwd, slug, steeringApplyBatch(map[string]any{
		"idempotencyKey": "k2",
		"ops":            []any{map[string]any{"kind": "nope"}},
	}), nil, SteerResultRejected)
	if _, err := os.Stat(lock); !os.IsNotExist(err) {
		t.Errorf("the lock survived a rejected batch: %v", err)
	}
}

// steeringApplyRetryDelays is a generous retry list: eight contenders in one process each hold
// the lock for several milliseconds, and the oracle's own list (5/10/20/40 ms, :47) is tuned for
// two. A run that exhausts it answers locked, and the case then says so.
func steeringApplyRetryDelays() []int {
	delays := make([]int, 200)
	for i := range delays {
		delays[i] = 2
	}
	return delays
}

// The lock spans the whole read-modify-write (:244-246), so concurrent batches with distinct keys
// all apply and no key is lost or recorded twice. This is the case that runs under -race.
func TestSteeringApplyConcurrentBatchesKeepEveryKey(t *testing.T) {
	const writers = 8
	cwd, slug := steeringApplyWorkspace(t)
	results := make([]SteerResult, writers)
	errs := make([]error, writers)
	var wg sync.WaitGroup
	for i := range writers {
		wg.Go(func() {
			results[i], errs[i] = ApplySteeringBatch(cwd, slug,
				steeringApplyBatch(map[string]any{"idempotencyKey": fmt.Sprintf("k-%d", i)}),
				&SteeringBatchOptions{Lock: &GoalplanWriteLockOptions{RetryDelaysMs: steeringApplyRetryDelays()}})
		})
	}
	wg.Wait()
	for i := range writers {
		if errs[i] != nil {
			t.Fatalf("writer %d: %v", i, errs[i])
		}
		if results[i].Kind != SteerResultApplied {
			t.Fatalf("writer %d: kind = %q (reason %q)", i, results[i].Kind, results[i].Reason)
		}
	}
	stored := ReadGoalplan(cwd, slug)
	if stored == nil || len(stored.SteeringLog) != writers {
		t.Fatalf("steeringLog = %#v", stored)
	}
	seen := map[string]int{}
	for _, entry := range stored.SteeringLog {
		seen[entry.IdempotencyKey]++
	}
	for i := range writers {
		if key := fmt.Sprintf("k-%d", i); seen[key] != 1 {
			t.Errorf("key %s appears %d times", key, seen[key])
		}
	}
	if got := steeringApplyEventRows(t, cwd, slug, EventSteered); got != writers {
		t.Errorf("steered rows = %d, want %d", got, writers)
	}
}
