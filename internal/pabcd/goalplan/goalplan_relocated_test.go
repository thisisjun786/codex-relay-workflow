package goalplan

// The CRW-856 cases: a plan write that published at the slug path and then found a DIFFERENT
// plan directory at that path must not be reported as published. The post-rename directory open
// runs boundFile, whose two identity failures (the descriptor is the wrong kind, or the
// descriptor's own path is not the expected one) mean the descriptor no longer names the plan
// directory at its path — the directory was moved or replaced after the rename. Those are
// returned as a plain error, so ApplySteeringBatch answers an error and appends no ledger row,
// and a retry applies to the plan now at the path. Every other post-rename open failure, and the
// directory fsync failure, still published the plan and stay *state.PublishedError (CRW-793).
//
// The seams are function fields of goalplanPublishedOptions, an argument the caller passes, never
// package state, so one case cannot fault another's write. The oracle never syncs after the
// rename, so it has no counterpart; these cases drive the Go port's own durability seam.
import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/state"
)

// goalplanRelocatedMoveAfterRename is the parent's reproduction (repro-718-p1): immediately after
// the rename, the plan directory is moved to <slug>.moved and the old plan is restored at the slug
// path. The descriptor the write holds now resolves to the moved directory, so the post-rename
// open reports the identity failure.
func goalplanRelocatedMoveAfterRename(t *testing.T, cwd, slug string) func() {
	t.Helper()
	dir := steeringApplyDir(t, cwd, slug)
	oldPlan, err := os.ReadFile(filepath.Join(dir, GoalplanFile))
	if err != nil {
		t.Fatalf("read the seeded plan: %v", err)
	}
	return func() {
		if err := os.Rename(dir, dir+".moved"); err != nil {
			t.Fatalf("move the plan directory: %v", err)
		}
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatalf("recreate the plan directory: %v", err)
		}
		if err := os.WriteFile(filepath.Join(dir, GoalplanFile), oldPlan, 0o600); err != nil {
			t.Fatalf("restore the old plan: %v", err)
		}
	}
}

// goalplanRelocatedOpenDirFails replaces the post-rename directory open with one that fails with
// the given cause, without touching the filesystem. It builds the same *os.PathError openAt
// builds, so the write path sees the shape a real open failure has.
func goalplanRelocatedOpenDirFails(cause error) *goalplanPublishedOptions {
	return &goalplanPublishedOptions{OpenDir: func(_ *os.File, expected string) (*os.File, error) {
		return nil, &os.PathError{Op: "open", Path: expected, Err: cause}
	}}
}

// The issue's first case: when the plan directory moved after the rename, the batch is answered
// with an error, not applied, and the ledger at the slug path gains no steered row. On dev the
// identity failure is wrapped as a PublishedError, so the answer is applied with a "could not be
// synced" warning and the steered row is written while the plan at the path lacks wp-new.
func TestGoalplanRelocatedAfterRenameIsRefused(t *testing.T) {
	cwd, slug := steeringApplyWorkspace(t)
	result, err := ApplySteeringBatch(cwd, slug, goalplanPublishedBatch(), &SteeringBatchOptions{
		Now:     func() string { return "2026-03-03T00:00:00.000Z" },
		publish: &goalplanPublishedOptions{AfterRename: goalplanRelocatedMoveAfterRename(t, cwd, slug)},
	})
	if err == nil {
		t.Fatalf("a relocated plan directory was answered %q, want an error", result.Kind)
	}
	if state.Published(err) {
		t.Errorf("a relocated plan directory is reported as published: %v", err)
	}
	if got := steeringApplyEventRows(t, cwd, slug, EventSteered); got != 0 {
		t.Errorf("steered rows at the slug path = %d, want 0", got)
	}
	// The plan at the slug path is the restored old one; the one that moved holds the write.
	if cur := ReadGoalplan(cwd, slug); cur != nil {
		for _, phase := range cur.WorkPhases {
			if phase.ID == "wp-new" {
				t.Errorf("the plan at the slug path holds the relocated work phase: %#v", cur.WorkPhases)
			}
		}
	}
}

// The issue's second case: after the refusal, the same batch sent again applies to the plan now at
// the path and writes exactly one steered row.
func TestGoalplanRelocatedRetryAppliesAtThePath(t *testing.T) {
	cwd, slug := steeringApplyWorkspace(t)
	if _, err := ApplySteeringBatch(cwd, slug, goalplanPublishedBatch(), &SteeringBatchOptions{
		Now:     func() string { return "2026-03-03T00:00:00.000Z" },
		publish: &goalplanPublishedOptions{AfterRename: goalplanRelocatedMoveAfterRename(t, cwd, slug)},
	}); err == nil {
		t.Fatal("the relocated write returned no error")
	}
	result, err := ApplySteeringBatch(cwd, slug, goalplanPublishedBatch(), &SteeringBatchOptions{
		Now: func() string { return "2026-03-03T00:01:00.000Z" },
	})
	if err != nil {
		t.Fatalf("retry ApplySteeringBatch: %v", err)
	}
	if result.Kind != SteerResultApplied {
		t.Fatalf("retry kind = %q (reason %q), want applied", result.Kind, result.Reason)
	}
	stored := ReadGoalplan(cwd, slug)
	if stored == nil || len(stored.SteeringLog) != 1 || stored.SteeringLog[0].IdempotencyKey != "k1" {
		t.Fatalf("the retry did not apply at the path: %#v", stored)
	}
	added := false
	for _, phase := range stored.WorkPhases {
		if phase.ID == "wp-new" {
			added = true
		}
	}
	if !added {
		t.Errorf("the retry plan does not hold the work phase: %#v", stored.WorkPhases)
	}
	if got := steeringApplyEventRows(t, cwd, slug, EventSteered); got != 1 {
		t.Errorf("steered rows = %d, want 1", got)
	}
}

// The regression (the evaluation's P2): a post-rename directory open that fails with EIO is not an
// identity failure, so the plan published and the batch stays applied with the durability warning
// and its one steered row.
func TestGoalplanRelocatedOpenFailureStaysPublished(t *testing.T) {
	cwd, slug := steeringApplyWorkspace(t)
	result, err := ApplySteeringBatch(cwd, slug, goalplanPublishedBatch(), &SteeringBatchOptions{
		Now:     func() string { return "2026-03-03T00:00:00.000Z" },
		publish: goalplanRelocatedOpenDirFails(syscall.EIO),
	})
	if err != nil {
		t.Fatalf("ApplySteeringBatch: %v", err)
	}
	if result.Kind != SteerResultApplied {
		t.Fatalf("kind = %q (reason %q), want applied", result.Kind, result.Reason)
	}
	if !strings.HasPrefix(result.Warning, "goalplan '"+slug+"' was published but its directory could not be synced: ") ||
		!strings.Contains(result.Warning, syscall.EIO.Error()) {
		t.Errorf("warning = %q, want the published-but-unsynced warning naming %v", result.Warning, syscall.EIO)
	}
	if got := steeringApplyEventRows(t, cwd, slug, EventSteered); got != 1 {
		t.Errorf("steered rows = %d, want 1", got)
	}
	if stored := ReadGoalplan(cwd, slug); stored == nil || len(stored.SteeringLog) != 1 {
		t.Fatalf("the published plan does not hold the entry: %#v", stored)
	}
}

// The write itself, without steering: a relocated plan directory is a plain error whose text is
// the issue's wording, not a *state.PublishedError. WriteGoalplan is a one-line delegate with no
// seam, so this is also what its other callers receive and fail closed on.
func TestGoalplanRelocatedWriteReportsTheMove(t *testing.T) {
	cwd := t.TempDir()
	plan := BuildGoalplan(NewGoalplanInput{Objective: "relocated write fixture"})
	if err := WriteGoalplan(cwd, plan); err != nil {
		t.Fatalf("seed plan: %v", err)
	}
	updated := ReadGoalplan(cwd, plan.Slug)
	if updated == nil {
		t.Fatal("the seeded plan did not read back")
	}
	updated.Objective = "relocated write fixture, updated"
	err := goalplanPublishedWriteGoalplan(cwd, updated, &goalplanPublishedOptions{
		AfterRename: goalplanRelocatedMoveAfterRename(t, cwd, plan.Slug),
	})
	if err == nil {
		t.Fatal("a relocated plan directory returned no error")
	}
	if state.Published(err) {
		t.Errorf("a relocated plan directory is reported as published: %v", err)
	}
	want := "goalplan '" + plan.Slug + "' directory moved after publication; the plan at its path is not the one written"
	if err.Error() != want {
		t.Errorf("error = %q, want %q", err.Error(), want)
	}
}

// The identity error is a typed error both of boundFile's identity answers share, so the writer's
// classification covers the wrong-kind failure and the descriptor mismatch alike, and errors.As
// finds it through the value openAt returns.
func TestGoalplanRelocatedIdentityErrorType(t *testing.T) {
	const text = "goalplan path is not a directory: /x"
	var identity error = &goalplanRelocatedError{msg: text}
	var relocated *goalplanRelocatedError
	if !errors.As(identity, &relocated) {
		t.Fatal("the identity error is not detected through errors.As")
	}
	if got := relocated.Error(); got != text {
		t.Errorf("text = %q, want %q", got, text)
	}
}

// The review finding on this PR (Devin red, Codex P2): a directory that moved after the rename AND
// is unreadable fails the post-rename open with EACCES, and the permission shortcut read that as a
// successful publication. The held descriptor's own identity is checked instead, so a relocated
// directory is refused however its open failed.
func TestGoalplanRelocatedUnreadableDirectoryIsRefused(t *testing.T) {
	cwd, slug := steeringApplyWorkspace(t)
	result, err := ApplySteeringBatch(cwd, slug, goalplanPublishedBatch(), &SteeringBatchOptions{
		Now: func() string { return "2026-03-03T00:00:00.000Z" },
		publish: &goalplanPublishedOptions{
			AfterRename: goalplanRelocatedMoveAfterRename(t, cwd, slug),
			OpenDir: func(*os.File, string) (*os.File, error) {
				return nil, &os.PathError{Op: "open", Path: ".", Err: syscall.EACCES}
			},
		},
	})
	if err == nil {
		t.Fatalf("a relocated, unreadable plan directory was answered %q, want an error", result.Kind)
	}
	if state.Published(err) {
		t.Errorf("a relocated, unreadable plan directory is reported as published: %v", err)
	}
	if got := steeringApplyEventRows(t, cwd, slug, EventSteered); got != 0 {
		t.Errorf("steered rows at the slug path = %d, want 0", got)
	}
}

// c1(3): a search/write-only plan directory that is still at its path keeps returning nil on the
// permission-denied open, so the batch stays applied with no warning and its one steered row.
func TestGoalplanRelocatedPermissionDeniedStaysPublished(t *testing.T) {
	cwd, slug := steeringApplyWorkspace(t)
	result, err := ApplySteeringBatch(cwd, slug, goalplanPublishedBatch(), &SteeringBatchOptions{
		Now:     func() string { return "2026-03-03T00:00:00.000Z" },
		publish: goalplanRelocatedOpenDirFails(syscall.EACCES),
	})
	if err != nil {
		t.Fatalf("ApplySteeringBatch: %v", err)
	}
	if result.Kind != SteerResultApplied || result.Warning != "" {
		t.Fatalf("kind = %q warning = %q, want applied with no warning", result.Kind, result.Warning)
	}
	if got := steeringApplyEventRows(t, cwd, slug, EventSteered); got != 1 {
		t.Errorf("steered rows = %d, want 1", got)
	}
}
