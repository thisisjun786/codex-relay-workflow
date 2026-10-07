package crwdir

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// These are CRW-936's cases for the returns that run after the exchange succeeded. On dev the move
// failure (swap.go:200-206) and the backup read-back failure (:207-209) return a PublishedError
// without syncing any directory, and the success path (:210-212) syncs only the target's directory,
// so a backup in another directory never reaches the disk. Every case here is red on that baseline.

// A colliding backup name after the reservation makes the no-replace move fail with EEXIST once the
// exchange has already run. The published rename is durable only if the directory is synced, so the
// sync must be called on this path too.
func TestCrwdirSwapSyncsTheDirectoryWhenTheMoveFails(t *testing.T) {
	dir := t.TempDir()
	target, backup := filepath.Join(dir, "config.toml"), swapBackup(dir)
	before := "model = \"a\"\n"
	writeSwapFile(t, target, before, 0o644)

	var synced []string
	displaced, err := crwdirSwapPublish(target, []byte(before), []byte("model = \"b\"\n"), backup, func(at crwdirSwapStep) error {
		if at != crwdirSwapStepMove {
			return nil
		}
		// The colliding name appears after the reservation and before the move.
		return os.WriteFile(backup, []byte("an operator's own file\n"), 0o644)
	}, func(at string) error { synced = append(synced, at); return nil })
	if !Published(err) {
		t.Fatalf("a post-exchange move failure was not reported as published: %v", err)
	}
	if displaced != nil {
		t.Fatalf("a failed backup read answered displaced content: %q", displaced)
	}
	if !slices.Contains(synced, dir) {
		t.Fatalf("the target's directory was not synced after the exchange: synced=%v", synced)
	}
	if got := read(t, target); got != "model = \"b\"\n" {
		t.Fatalf("the exchanged content is not in place: %q", got)
	}
}

// A sync failure on the move-failure path must not hide the move failure, and the move failure must
// not hide the sync failure: both stay answerable through the PublishedError.
func TestCrwdirSwapJoinsASyncFailureIntoTheMoveFailure(t *testing.T) {
	dir := t.TempDir()
	target, backup := filepath.Join(dir, "config.toml"), swapBackup(dir)
	before := "model = \"a\"\n"
	writeSwapFile(t, target, before, 0o644)
	injected := errors.New("injected directory sync failure")

	_, err := crwdirSwapPublish(target, []byte(before), []byte("model = \"b\"\n"), backup, func(at crwdirSwapStep) error {
		if at != crwdirSwapStepMove {
			return nil
		}
		return os.WriteFile(backup, []byte("an operator's own file\n"), 0o644)
	}, func(string) error { return injected })
	if !Published(err) {
		t.Fatalf("a post-exchange move failure was not reported as published: %v", err)
	}
	if !errors.Is(err, injected) {
		t.Fatalf("the sync failure was not joined into the move failure: %v", err)
	}
	if !errors.Is(err, os.ErrExist) {
		t.Fatalf("the move failure itself is no longer answerable: %v", err)
	}
	var published *PublishedError
	if !errors.As(err, &published) || published.DisplacedAt == "" {
		t.Fatalf("the failure does not name where the displaced content is: %v", err)
	}
	if got := read(t, published.DisplacedAt); got != before {
		t.Fatalf("the displaced content was deleted rather than kept at %s: %q", published.DisplacedAt, got)
	}
}

// The success path must sync the backup's directory too when it differs from the target's: the
// no-replace move that filled the backup is a second rename that has to survive a power failure.
func TestCrwdirSwapSyncsTheBackupsDirectoryWhenItDiffers(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "config.toml")
	backupDir := t.TempDir()
	backup := filepath.Join(backupDir, "config.toml.bak-2026-01-01T00-00-00.000Z")
	before := "model = \"a\"\n"
	next := "model = \"b\"\n"
	writeSwapFile(t, target, before, 0o644)

	var synced []string
	displaced, err := crwdirSwapPublish(target, []byte(before), []byte(next), backup, nil, func(at string) error {
		synced = append(synced, at)
		return nil
	})
	if err != nil {
		t.Fatalf("the publication failed: %v", err)
	}
	if string(displaced) != before {
		t.Fatalf("displaced %q, want %q", displaced, before)
	}
	if !slices.Contains(synced, dir) {
		t.Fatalf("the target's directory was not synced: synced=%v", synced)
	}
	if !slices.Contains(synced, backupDir) {
		t.Fatalf("the backup's directory was not synced: synced=%v", synced)
	}
	if got := read(t, backup); got != before {
		t.Fatalf("the backup holds %q, want %q", got, before)
	}
}

// The backup read-back failure is the second post-exchange return the issue names. It has no step
// between the successful move and the read, so it needs its own seam; the published rename must
// still be made durable before the failure is reported.
func TestCrwdirSwapSyncsTheDirectoryWhenTheBackupReadFails(t *testing.T) {
	dir := t.TempDir()
	target, backup := filepath.Join(dir, "config.toml"), swapBackup(dir)
	before := "model = \"a\"\n"
	writeSwapFile(t, target, before, 0o644)
	injected := errors.New("injected backup read failure")

	var synced []string
	displaced, err := crwdirSwapPublish(target, []byte(before), []byte("model = \"b\"\n"), backup, func(at crwdirSwapStep) error {
		if at == crwdirSwapStepReadBackup {
			return injected
		}
		return nil
	}, func(at string) error { synced = append(synced, at); return nil })
	if !Published(err) {
		t.Fatalf("a post-exchange backup read failure was not reported as published: %v", err)
	}
	if !errors.Is(err, injected) {
		t.Fatalf("the read failure was not surfaced: %v", err)
	}
	if displaced != nil {
		t.Fatalf("a failed backup read answered displaced content: %q", displaced)
	}
	if !slices.Contains(synced, dir) {
		t.Fatalf("the target's directory was not synced after the exchange: synced=%v", synced)
	}
	var published *PublishedError
	if !errors.As(err, &published) || published.DisplacedAt != backup {
		t.Fatalf("the failure does not name where the displaced content is: %v", err)
	}
	if got := read(t, backup); got != before {
		t.Fatalf("the displaced content is not in the backup: %q", got)
	}
}
