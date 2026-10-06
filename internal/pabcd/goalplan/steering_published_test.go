package goalplan

// The CRW-793 cases: a goalplan write that published at the final path and then failed to sync the plan
// directory is still a written plan. The oracle never fsyncs the published file or its directory
// (known-defects.md, "Found by CRW-479 state publication durability"), so it cannot fail after the
// rename and has no recorded case here; these cases drive the Go port's own durability seam, which is a
// function argument the caller passes, never a package-level variable.
//
// The seam fails only the directory sync, by descriptor kind, exactly as state's
// TestStateDirectorySyncFailureReturnsErrorAndKeepsPublication does through writeState's syncFile.
import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/state"
)

// goalplanPublishedDirectorySync is the CRW-793 seam: it syncs a regular file for real and fails every
// directory with syscall.EIO. The descriptor kind is the only discriminator the write path offers, and it
// is enough: the staged plan is a regular file and the post-rename reader is opened with O_DIRECTORY.
func goalplanPublishedDirectorySync(calls *[]string) *goalplanPublishedOptions {
	return &goalplanPublishedOptions{Sync: func(f *os.File) error {
		info, err := f.Stat()
		if err != nil {
			return err
		}
		if info.IsDir() {
			*calls = append(*calls, "directory")
			return syscall.EIO
		}
		*calls = append(*calls, "file")
		return f.Sync()
	}}
}

// goalplanPublishedFileSync fails the staged file's sync and nothing else, so nothing is published.
func goalplanPublishedFileSync() *goalplanPublishedOptions {
	return &goalplanPublishedOptions{Sync: func(f *os.File) error { return syscall.EIO }}
}

// goalplanPublishedBatch is the issue's reproduction: an add-work-phase batch that also declares a
// prerequisite, so the ledger must gain both the steered row and the dependency_registered row.
func goalplanPublishedBatch() map[string]any {
	return map[string]any{
		"idempotencyKey": "k1",
		"rationale":      "add work",
		"evidence":       "issue",
		"ops": []any{map[string]any{
			"kind": "add-work-phase", "id": "wp-new", "title": "New work",
		}},
	}
}

// The issue's first case: with only the directory sync failing, the batch is applied, the plan holds the
// key and the ledger holds the row the batch promised. On dev the write error is returned as a plain
// failure, so the answer is an error and the steered row is never written.
func TestGoalplanPublishedSteeringApplyWritesTheLedgerRow(t *testing.T) {
	cwd, slug := steeringApplyWorkspace(t)
	calls := []string{}
	result, err := ApplySteeringBatch(cwd, slug, goalplanPublishedBatch(), &SteeringBatchOptions{
		Now:     func() string { return "2026-03-03T00:00:00.000Z" },
		publish: goalplanPublishedDirectorySync(&calls),
	})
	if err != nil {
		t.Fatalf("ApplySteeringBatch: %v", err)
	}
	if result.Kind != SteerResultApplied {
		t.Fatalf("kind = %q (reason %q), want applied", result.Kind, result.Reason)
	}
	if result.Plan == nil || result.Entry == nil {
		t.Fatalf("applied without plan/entry: %#v", result)
	}
	want := "goalplan '" + slug + "' was published but its directory could not be synced: " + syscall.EIO.Error()
	if result.Warning != want {
		t.Errorf("warning = %q, want %q", result.Warning, want)
	}
	stored := ReadGoalplan(cwd, slug)
	if stored == nil || len(stored.SteeringLog) != 1 || stored.SteeringLog[0].IdempotencyKey != "k1" {
		t.Fatalf("steeringLog = %#v", stored)
	}
	added := false
	for _, phase := range stored.WorkPhases {
		if phase.ID == "wp-new" {
			added = true
		}
	}
	if !added {
		t.Errorf("the published plan does not hold the new work phase: %#v", stored.WorkPhases)
	}
	if got := steeringApplyEventRows(t, cwd, slug, EventSteered); got != 1 {
		t.Errorf("steered rows = %d, want 1", got)
	}
	if got := steeringApplyEventRows(t, cwd, slug, EventDependencyRegistered); got != 0 {
		t.Errorf("dependency_registered rows = %d, want 0 for a phase with no dependsOn", got)
	}
	if len(calls) != 2 || calls[0] != "file" || calls[1] != "directory" {
		t.Fatalf("sync calls = %v, want [file directory]", calls)
	}
}

// A batch that declares a prerequisite also records its dependency row after a published write.
func TestGoalplanPublishedSteeringApplyWritesTheDependencyRow(t *testing.T) {
	cwd, slug := steeringApplyWorkspace(t)
	seeded := ReadGoalplan(cwd, slug)
	if seeded == nil {
		t.Fatal("the seeded plan did not read back")
	}
	seeded.WorkPhases = []GoalplanWorkPhase{
		{ID: "wp-a", Title: "A", Status: WorkPhaseDone, Tasks: []GoalplanTask{}, CriteriaIDs: []string{}},
	}
	if err := WriteGoalplan(cwd, seeded); err != nil {
		t.Fatalf("seed the phase: %v", err)
	}
	calls := []string{}
	result, err := ApplySteeringBatch(cwd, slug, map[string]any{
		"idempotencyKey": "k1",
		"rationale":      "add work",
		"evidence":       "issue",
		"ops": []any{map[string]any{
			"kind": "add-work-phase", "id": "wp-new", "title": "New work", "dependsOn": []any{"wp-a"},
		}},
	}, &SteeringBatchOptions{Now: func() string { return "2026-03-03T00:00:00.000Z" }, publish: goalplanPublishedDirectorySync(&calls)})
	if err != nil {
		t.Fatalf("ApplySteeringBatch: %v", err)
	}
	if result.Kind != SteerResultApplied || result.Warning == "" {
		t.Fatalf("kind = %q warning = %q", result.Kind, result.Warning)
	}
	if got := steeringApplyEventRows(t, cwd, slug, EventSteered); got != 1 {
		t.Errorf("steered rows = %d, want 1", got)
	}
	if got := steeringApplyEventRows(t, cwd, slug, EventDependencyRegistered); got != 1 {
		t.Errorf("dependency_registered rows = %d, want 1", got)
	}
}

// The issue's second case: the key is recorded, so the retry is a no-op and the ledger keeps its one row.
func TestGoalplanPublishedSteeringRetryIsDuplicate(t *testing.T) {
	cwd, slug := steeringApplyWorkspace(t)
	calls := []string{}
	steeringApply(t, cwd, slug, goalplanPublishedBatch(), &SteeringBatchOptions{
		Now:     func() string { return "2026-03-03T00:00:00.000Z" },
		publish: goalplanPublishedDirectorySync(&calls),
	}, SteerResultApplied)
	before := steeringApplyLedgerText(t, cwd, slug)
	again := steeringApply(t, cwd, slug, goalplanPublishedBatch(), nil, SteerResultDuplicate)
	if again.Entry == nil || again.Entry.IdempotencyKey != "k1" {
		t.Fatalf("duplicate entry = %#v", again.Entry)
	}
	if again.Warning != "" {
		t.Errorf("duplicate warning = %q, want none", again.Warning)
	}
	if ledger := steeringApplyLedgerText(t, cwd, slug); ledger != before {
		t.Errorf("the duplicate wrote a ledger line: %q", ledger)
	}
	if got := steeringApplyEventRows(t, cwd, slug, EventSteered); got != 1 {
		t.Errorf("steered rows = %d, want 1", got)
	}
}

// The other side of the seam: a failure before the rename published nothing, so it stays a plain error
// and leaves the plan's bytes at the final path untouched.
func TestGoalplanPublishedPreRenameFailureIsAnError(t *testing.T) {
	cwd, slug := steeringApplyWorkspace(t)
	before := steeringApplyPlanText(t, cwd, slug)
	result, err := ApplySteeringBatch(cwd, slug, goalplanPublishedBatch(),
		&SteeringBatchOptions{Now: func() string { return "2026-03-03T00:00:00.000Z" }, publish: goalplanPublishedFileSync()})
	if err == nil {
		t.Fatalf("a pre-rename failure returned no error: %#v", result)
	}
	if state.Published(err) {
		t.Errorf("a pre-rename failure is reported as published: %v", err)
	}
	if !errors.Is(err, syscall.EIO) {
		t.Errorf("errors.Is(err, syscall.EIO) is false: %v", err)
	}
	if after := steeringApplyPlanText(t, cwd, slug); after != before {
		t.Errorf("the plan bytes changed: %q", after)
	}
	dir := steeringApplyDir(t, cwd, slug)
	entries, readErr := os.ReadDir(dir)
	if readErr != nil {
		t.Fatalf("read plan dir: %v", readErr)
	}
	for _, entry := range entries {
		if strings.HasSuffix(entry.Name(), ".tmp") {
			t.Errorf("a temp file survived the failed write: %s", filepath.Join(dir, entry.Name()))
		}
	}
}

// The write itself, without the steering transaction around it: a directory sync failure after the
// rename is reported as CRW-744's PublishedError with the cause still reachable, and the plan at the
// final path is the new one. WriteGoalplan is a one-line delegate to this function with no seam, so
// this is also what its callers receive.
func TestGoalplanPublishedWriteReportsPublished(t *testing.T) {
	cwd := t.TempDir()
	plan := BuildGoalplan(NewGoalplanInput{Objective: "published write fixture"})
	if err := WriteGoalplan(cwd, plan); err != nil {
		t.Fatalf("seed plan: %v", err)
	}
	before := steeringApplyPlanText(t, cwd, plan.Slug)
	updated := ReadGoalplan(cwd, plan.Slug)
	if updated == nil {
		t.Fatal("the seeded plan did not read back")
	}
	updated.Objective = "published write fixture, updated"
	calls := []string{}
	err := goalplanPublishedWriteGoalplan(cwd, updated, goalplanPublishedDirectorySync(&calls))
	if err == nil {
		t.Fatal("a directory sync failure returned no error")
	}
	if !state.Published(err) {
		t.Fatalf("a post-rename failure is not reported as published: %v", err)
	}
	if !errors.Is(err, syscall.EIO) {
		t.Fatalf("errors.Is(err, syscall.EIO) is false through Unwrap: %v", err)
	}
	if len(calls) != 2 || calls[0] != "file" || calls[1] != "directory" {
		t.Fatalf("sync calls = %v, want [file directory]", calls)
	}
	after := steeringApplyPlanText(t, cwd, plan.Slug)
	if after == before {
		t.Error("the published plan at the final path is still the old one")
	}
	if stored := ReadGoalplan(cwd, plan.Slug); stored == nil || stored.Objective != "published write fixture, updated" {
		t.Fatalf("the plan at the final path does not hold the new objective: %#v", stored)
	}
}
