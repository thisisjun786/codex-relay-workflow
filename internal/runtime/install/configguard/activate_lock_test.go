package configguard

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/crwdir"
)

// CRW-844: the activation read-modify-write of config.toml takes the same sidecar lock retrust takes,
// so a CRW writer holding it makes the activation refuse rather than interleave on one config.toml.
// Every other file this package publishes keeps activationPublish.
func TestActivateTakesTheConfigLock(t *testing.T) {
	home := activationHome(t)
	path := filepath.Join(home, "config.toml")
	activationWrite(t, path, "[features]\nhooks = false\n")

	// A CRW writer holding the lock makes the activation itself refuse, with the shared busy message
	// and nothing written; the test drives the real Activate, not just LockConfig.
	held, err := crwdir.LockConfig(path, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	var calls [][]string
	if _, e := Activate(activationDeps(t, home, allActivationFlags(), &calls)); e == nil || !strings.Contains(e.Error(), crwdir.ConfigLockBusy) {
		t.Fatalf("the activation did not refuse while the lock was held: %v", e)
	}
	if got := activationRead(t, path); got != "[features]\nhooks = false\n" {
		t.Fatalf("the refused activation wrote: %q", got)
	}
	held.Release()

	// The released lock lets the activation through.
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
