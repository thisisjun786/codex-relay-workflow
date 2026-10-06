package crwdir

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"
)

// These are CRW-844's cases for the swap publication. The step hook is what makes the interleaving
// testable: the last check and the exchange are two steps inside one call, so a test cannot
// otherwise put a save between them.

// swapBackup is the backup name the tests use; the caller (retrust) picks the real one.
func swapBackup(dir string) string {
	return filepath.Join(dir, "config.toml.bak-2026-01-01T00-00-00.000Z")
}

func writeSwapFile(t *testing.T, path, content string, mode os.FileMode) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
}

// A save that lands after the last check but before the exchange is the P0 this change fixes. The
// content that was there is the file the exchange displaced, so it is kept in the backup and the
// publication answers it; nothing is exchanged back and nothing is deleted.
func TestCrwdirSwapKeepsASaveFromBeforeTheExchange(t *testing.T) {
	dir := t.TempDir()
	target, backup := filepath.Join(dir, "config.toml"), swapBackup(dir)
	before := "model = \"a\"\n"
	raced := "model = \"b\"\n"
	next := "model = \"a\"\n[hooks.state]\n"
	writeSwapFile(t, target, before, 0o644)

	displaced, err := crwdirSwapPublish(target, []byte(before), []byte(next), backup, func(at crwdirSwapStep) error {
		if at != crwdirSwapStepCompare {
			return nil
		}
		// The compare has run against the bytes retrust read; the non-cooperative writer saves now.
		writeSwapFile(t, target, raced, 0o644)
		return nil
	}, nil)
	if err != nil {
		t.Fatalf("the publication failed: %v", err)
	}
	if string(displaced) != raced {
		t.Fatalf("the displaced content was reported as %q, want %q", displaced, raced)
	}
	if got := read(t, target); got != next {
		t.Fatalf("the target holds %q, want retrust's content %q", got, next)
	}
	if got := read(t, backup); got != raced {
		t.Fatalf("the raced save is not in the backup: %q", got)
	}
	if !slices.Equal(names(t, dir), []string{"config.toml", "config.toml.bak-2026-01-01T00-00-00.000Z"}) {
		t.Fatalf("the directory holds %v", names(t, dir))
	}
}

// The cooperative case: nothing changed, so the displaced content is exactly what retrust read and
// the backup holds it.
func TestCrwdirSwapPublishesAndKeepsTheDisplacedContent(t *testing.T) {
	dir := t.TempDir()
	target, backup := filepath.Join(dir, "config.toml"), swapBackup(dir)
	before := "model = \"a\"\n"
	next := "model = \"a\"\n[hooks.state]\n"
	writeSwapFile(t, target, before, 0o640)

	displaced, err := crwdirSwapPublish(target, []byte(before), []byte(next), backup, nil, nil)
	if err != nil {
		t.Fatalf("the publication failed: %v", err)
	}
	if string(displaced) != before {
		t.Fatalf("displaced %q, want %q", displaced, before)
	}
	if got := read(t, target); got != next || modeOf(t, target) != 0o640 {
		t.Fatalf("the target is %q mode %v", got, modeOf(t, target))
	}
	if got := read(t, backup); got != before {
		t.Fatalf("the backup holds %q, want %q", got, before)
	}
}

// A filesystem without an atomic exchange is refused before anything is written: there is no
// plain-rename fallback, because a plain rename cannot keep the file it displaced.
func TestCrwdirSwapRefusesWithoutAnExchange(t *testing.T) {
	dir := t.TempDir()
	target, backup := filepath.Join(dir, "config.toml"), swapBackup(dir)
	before := "model = \"a\"\n"
	writeSwapFile(t, target, before, 0o644)

	_, err := crwdirSwapPublish(target, []byte(before), []byte("model = \"b\"\n"), backup, func(at crwdirSwapStep) error {
		if at == crwdirSwapStepExchange {
			return &os.LinkError{Op: "rename", Old: "a", New: "b", Err: syscall.EINVAL}
		}
		return nil
	}, nil)
	if err == nil || !errors.Is(err, syscall.EINVAL) {
		t.Fatalf("an unsupported exchange was not refused: %v", err)
	}
	if got := read(t, target); got != before {
		t.Fatalf("the refusal changed the target: %q", got)
	}
	if _, statErr := os.Stat(backup); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("the refusal left a backup: %v", statErr)
	}
	if !slices.Equal(names(t, dir), []string{"config.toml"}) {
		t.Fatalf("the refusal left %v", names(t, dir))
	}
}

// A failure after the exchange is a PublishedError: the new content is visible, so the caller counts
// the publication as done and reports the failure rather than undoing it.
func TestCrwdirSwapDirectorySyncFailureIsPublished(t *testing.T) {
	dir := t.TempDir()
	target, backup := filepath.Join(dir, "config.toml"), swapBackup(dir)
	before := "model = \"a\"\n"
	next := "model = \"b\"\n"
	writeSwapFile(t, target, before, 0o644)
	injected := errors.New("injected directory sync failure")

	displaced, err := crwdirSwapPublish(target, []byte(before), []byte(next), backup, nil, func(string) error { return injected })
	if !Published(err) || !errors.Is(err, injected) {
		t.Fatalf("a sync failure is not a published failure: %v", err)
	}
	if string(displaced) != before {
		t.Fatalf("displaced %q, want %q", displaced, before)
	}
	if got := read(t, target); got != next {
		t.Fatalf("the published content was undone: %q", got)
	}
	if got := read(t, backup); got != before {
		t.Fatalf("the backup holds %q, want %q", got, before)
	}
}

// An occupied backup path refuses the whole publication before the exchange, as the oracle's
// exclusive copy did: nothing is exchanged, the target keeps the bytes the plan was computed from,
// and the operator's file is untouched. This is the common collision (a second run within the same
// timestamped name) and it must not leave the target replaced with the displaced content only at a
// temporary path.
func TestCrwdirSwapRefusesAnOccupiedBackupBeforeTheExchange(t *testing.T) {
	dir := t.TempDir()
	target, backup := filepath.Join(dir, "config.toml"), swapBackup(dir)
	before := "model = \"a\"\n"
	existing := "an operator's own file\n"
	writeSwapFile(t, target, before, 0o644)
	writeSwapFile(t, backup, existing, 0o644)

	displaced, err := crwdirSwapPublish(target, []byte(before), []byte("model = \"b\"\n"), backup, nil, nil)
	if err == nil || !errors.Is(err, os.ErrExist) {
		t.Fatal("an occupied backup path was replaced")
	}
	if Published(err) {
		t.Fatalf("a refusal before the exchange was reported as published: %v", err)
	}
	if displaced != nil {
		t.Fatalf("a refused publication answered displaced content: %q", displaced)
	}
	if got := read(t, backup); got != existing {
		t.Fatalf("the operator's file was overwritten: %q", got)
	}
	if got := read(t, target); got != before {
		t.Fatalf("the target was replaced by a refused publication: %q", got)
	}
	if !slices.Equal(names(t, dir), []string{"config.toml", "config.toml.bak-2026-01-01T00-00-00.000Z"}) {
		t.Fatalf("the refusal left %v", names(t, dir))
	}
}

// A backup path taken between the reservation and the move is the residual window the no-replace
// move covers: the exchange has run, so the failure is a PublishedError, the new content is in
// place, the displaced content is still at the path the error names, and nothing is deleted.
func TestCrwdirSwapKeepsTheDisplacedContentWhenTheMoveFails(t *testing.T) {
	dir := t.TempDir()
	target, backup := filepath.Join(dir, "config.toml"), swapBackup(dir)
	before := "model = \"a\"\n"
	next := "model = \"b\"\n"
	writeSwapFile(t, target, before, 0o644)

	displaced, err := crwdirSwapPublish(target, []byte(before), []byte(next), backup, func(at crwdirSwapStep) error {
		if at != crwdirSwapStepMove {
			return nil
		}
		// A colliding name appears after the reservation and before the move.
		return os.WriteFile(backup, []byte("an operator's own file\n"), 0o644)
	}, nil)
	if !Published(err) {
		t.Fatalf("a post-exchange move failure was not reported as published: %v", err)
	}
	var published *PublishedError
	if !errors.As(err, &published) || published.DisplacedAt == "" {
		t.Fatalf("the failure does not name where the displaced content is: %v", err)
	}
	if displaced != nil {
		t.Fatalf("a failed backup read answered displaced content: %q", displaced)
	}
	if got := read(t, target); got != next {
		t.Fatalf("the exchanged content is not in place: %q", got)
	}
	if got := read(t, published.DisplacedAt); got != before {
		t.Fatalf("the displaced content was deleted rather than kept at %s: %q", published.DisplacedAt, got)
	}
	if got := read(t, backup); got != "an operator's own file\n" {
		t.Fatalf("the colliding file was overwritten: %q", got)
	}
}

// The sidecar lock serializes CRW writers: a second taker waits, and a holder that keeps the lock
// makes the second answer busy rather than publish.
func TestCrwdirConfigLockIsExclusive(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "config.toml")
	writeSwapFile(t, target, "model = \"a\"\n", 0o644)
	held, err := LockConfig(target, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := LockConfig(target, 0); err == nil || err.Error() != ConfigLockBusy {
		t.Fatalf("a second taker was not refused: %v", err)
	}
	held.Release()
	again, err := LockConfig(target, time.Second)
	if err != nil {
		t.Fatalf("the lock was not released: %v", err)
	}
	if again.Target != target {
		t.Fatalf("the lock resolved to %q, want %q", again.Target, target)
	}
	again.Release()
	// The sidecar is never unlinked (docs/port/decisions.md 7).
	if _, err := os.Stat(target + crwdirSwapLockSuffix); err != nil {
		t.Fatalf("the sidecar was removed: %v", err)
	}
}

// A wait lets a short holder finish rather than refusing at once.
func TestCrwdirConfigLockWaitsForAShortHolder(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "config.toml")
	writeSwapFile(t, target, "model = \"a\"\n", 0o644)
	held, err := LockConfig(target, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		time.Sleep(50 * time.Millisecond)
		held.Release()
		close(done)
	}()
	waited, err := LockConfig(target, 5*time.Second)
	if err != nil {
		t.Fatalf("the waiter did not take the lock: %v", err)
	}
	waited.Release()
	<-done
}

// EPERM at the exchange is a real permission refusal, not "this filesystem cannot exchange": it must
// not be reported as a filesystem limitation that tells the operator to give up.
func TestCrwdirSwapDoesNotCallAPermissionFailureUnsupported(t *testing.T) {
	dir := t.TempDir()
	target, backup := filepath.Join(dir, "config.toml"), swapBackup(dir)
	before := "model = \"a\"\n"
	writeSwapFile(t, target, before, 0o644)

	_, err := crwdirSwapPublish(target, []byte(before), []byte("model = \"b\"\n"), backup, func(at crwdirSwapStep) error {
		if at == crwdirSwapStepExchange {
			return &os.LinkError{Op: "rename", Old: "a", New: "b", Err: syscall.EPERM}
		}
		return nil
	}, nil)
	if err == nil || !errors.Is(err, syscall.EPERM) {
		t.Fatalf("the permission failure was not surfaced: %v", err)
	}
	if strings.Contains(err.Error(), "does not support an atomic exchange") {
		t.Fatalf("a permission failure was reported as an unsupported filesystem: %v", err)
	}
	if got := read(t, target); got != before {
		t.Fatalf("the refusal changed the target: %q", got)
	}
}
