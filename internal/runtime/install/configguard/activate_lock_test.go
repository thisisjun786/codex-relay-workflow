package configguard

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/crwdir"
)

// CRW-844: the activation publish of config.toml takes the same sidecar lock retrust takes, so a
// CRW writer holding it makes the activation wait (and, past the wait, refuse) rather than
// interleave on one config.toml. Every other file this package publishes keeps activationPublish.
func TestActivateTakesTheConfigLock(t *testing.T) {
	home := activationHome(t)
	path := filepath.Join(home, "config.toml")
	activationWrite(t, path, "[features]\nhooks = false\n")

	held, err := crwdir.LockConfig(path, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	// A waiter with a short deadline refuses with the shared busy message rather than writing.
	if _, err := crwdir.LockConfig(path, 0); err == nil || !strings.Contains(err.Error(), crwdir.ConfigLockBusy) {
		t.Fatalf("the lock was not exclusive: %v", err)
	}
	held.Release()

	var calls [][]string
	if _, e := Activate(activationDeps(t, home, allActivationFlags(), &calls)); e != nil {
		t.Fatalf("the activation did not take the released lock: %v", e)
	}
	// The sidecar is left in place, never unlinked (docs/port/decisions.md 7).
	if _, e := os.Stat(path + ".crw-lock"); e != nil {
		t.Fatalf("the sidecar was removed: %v", e)
	}
	// The manifest is not config.toml and keeps the unlocked publish.
	if _, e := os.Stat(manifestPath(home)); e != nil {
		t.Fatalf("the manifest was not published: %v", e)
	}
	if _, e := os.Stat(manifestPath(home) + ".crw-lock"); !os.IsNotExist(e) {
		t.Fatalf("the manifest publish took the config lock: %v", e)
	}
}
