package configguard

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/crwdir"
)

// A file that is in place but whose directory could not be synced is a completed publication for the activation
// writers: they go on to the files that depend on it, and a failure before the rename is still returned (CRW-802).
func TestActivationPublishCountsAnUnsyncedPublicationAsDone(t *testing.T) {
	saved := activationCrwdirPublish
	t.Cleanup(func() { activationCrwdirPublish = saved })
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	activationCrwdirPublish = func(p string, b []byte) error {
		if err := os.WriteFile(p, b, 0o644); err != nil {
			return err
		}
		return &crwdir.PublishedError{Err: errors.New("injected directory sync failure")}
	}
	if err := activationPublish(path, []byte("a = 1\n")); err != nil {
		t.Fatalf("an unsynced publication: %v", err)
	}
	if b, err := os.ReadFile(path); err != nil || string(b) != "a = 1\n" {
		t.Fatalf("content %q, %v", b, err)
	}
	injected := errors.New("injected write failure")
	activationCrwdirPublish = func(string, []byte) error { return injected }
	if err := activationPublish(path, []byte("b = 2\n")); !errors.Is(err, injected) {
		t.Fatalf("a failure before the rename: %v", err)
	}
}
