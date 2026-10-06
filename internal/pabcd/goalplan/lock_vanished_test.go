//go:build linux

package goalplan

// The acquisition path's vanished-lock cases. The race the issue reads from the failure
// message: a writer waiting on the lock directory opens it, and the holder releases and
// removes it between that openat and boundFile's descriptor check, so the check reports the
// expected path with a " (deleted)" suffix and the acquisition fails instead of retrying.
// The seam below orders that release into the window without a sleep, which the oracle's
// own mkdir retry (goalplan.ts:806-873) handles by simply retrying.
import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestLockVanishedRetriesAcquisition forces the issue's order: the waiter opens the held
// lock directory, then the holder removes it. On the unfixed acquisition path the descriptor
// check refuses the vanished lock and the call fails; after the fix the acquisition retries
// within the existing budget and the callback runs.
func TestLockVanishedRetriesAcquisition(t *testing.T) {
	cwd, dir := readWorkspace(t)
	lock := filepath.Join(dir, GoalplanLockDir)
	if err := os.Mkdir(lock, 0o700); err != nil {
		t.Fatal(err)
	}
	var delays []int
	var opens int
	goalplanLockVanishedAfterHeldOpen = func() {
		opens++
		if opens == 1 {
			if err := os.Remove(lock); err != nil {
				t.Errorf("remove held lock: %v", err)
			}
		}
	}
	defer func() { goalplanLockVanishedAfterHeldOpen = nil }()
	ran := false
	got, err := WithGoalplanWriteLock(cwd, "demo", func(*Goalplan) (int, error) {
		ran = true
		return 7, nil
	}, &GoalplanWriteLockOptions{RetryDelaysMs: []int{0, 0, 0, 0}, Sleep: func(ms int) { delays = append(delays, ms) }})
	if err != nil {
		t.Fatalf("vanished lock was not retried: %v", err)
	}
	if got.Kind != "ok" || got.Value == nil || *got.Value != 7 || !ran {
		t.Fatalf("acquisition did not complete: %+v ran=%v", got, ran)
	}
	if len(delays) == 0 {
		t.Fatalf("the vanished lock consumed no retry delay")
	}
	if _, err := os.Stat(lock); !os.IsNotExist(err) {
		t.Fatalf("the lock was not released: %v", err)
	}
}

// TestLockVanishedOtherPathRefused keeps the path-swap defence: a lock whose descriptor names
// any other path is still an error, not a vanished lock. A cooperating writer may not relocate
// a live lock (open.go:14-15); this is the adversarial shape the seam can still produce.
func TestLockVanishedOtherPathRefused(t *testing.T) {
	for _, kind := range []string{"renamed", "renamed-removed"} {
		t.Run(kind, func(t *testing.T) {
			cwd, dir := readWorkspace(t)
			lock := filepath.Join(dir, GoalplanLockDir)
			if err := os.Mkdir(lock, 0o700); err != nil {
				t.Fatal(err)
			}
			moved := filepath.Join(dir, "moved-lock")
			goalplanLockVanishedAfterHeldOpen = func() {
				if err := os.Rename(lock, moved); err != nil {
					t.Errorf("rename held lock: %v", err)
					return
				}
				if kind == "renamed-removed" {
					if err := os.Remove(moved); err != nil {
						t.Errorf("remove moved lock: %v", err)
					}
				}
			}
			defer func() { goalplanLockVanishedAfterHeldOpen = nil }()
			got, err := WithGoalplanWriteLock(cwd, "demo", func(*Goalplan) (int, error) { return 0, nil },
				&GoalplanWriteLockOptions{RetryDelaysMs: []int{}, Sleep: func(int) {}})
			if err == nil {
				t.Fatalf("a lock naming another path was accepted: %+v", got)
			}
			if !strings.Contains(err.Error(), "goalplan descriptor path") {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}
