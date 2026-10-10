package configguard

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/crwdir"
)

// CRW-1144: when the CLI replaces the caller's config.toml while the activation holds the lock on the file it named before,
// another writer that reaches the new file must not be able to write it at the same time as the activation: the activation
// either takes the new file's lock too or publishes nothing more.
func TestActivateDoesNotPublishUnderALockItNoLongerHolds(t *testing.T) {
	home := configLockActivationHome(t)
	path := filepath.Join(home, "config.toml")
	target := filepath.Join(home, "target.toml")
	activationWrite(t, target, configLockPathsPre)
	if err := os.Symlink("target.toml", path); err != nil {
		t.Fatal(err)
	}
	var calls [][]string
	runner := configLockPathsRenameRunner(t, path, &calls)
	var other *crwdir.ConfigLock
	deps := ActivateDeps{CodexHome: home, Now: func() string { return "2026-06-30T00:00:00.000Z" }, Run: func(args []string) CodexRunResult {
		res := runner(args)
		if args[1] == "enable" && other == nil {
			// Another CRW writer reaches the replaced file and takes its lock while the activation is still running.
			var err error
			if other, err = crwdir.LockConfig(path, 0); err != nil {
				t.Fatalf("the other writer could not lock the new file: %v", err)
			}
		}
		return res
	}}
	m, err := Activate(deps)
	if other == nil {
		t.Fatal("the runner did not replace config.toml")
	}
	holding := activationRead(t, path)
	other.Release()
	if err == nil && strings.Contains(holding, "dedicated_tools") {
		t.Fatalf("the activation published the managed key while another writer held the new file's lock: %+v", m)
	}
	if err == nil {
		t.Fatal("the activation reported success without the key")
	}
	// The retry records what the first run did and finishes under the new file's lock.
	if _, err := Activate(ActivateDeps{CodexHome: home, Run: runner, Now: deps.Now}); err != nil {
		t.Fatalf("retry: %v", err)
	}
	if got := activationRead(t, path); !strings.Contains(got, "dedicated_tools = true") {
		t.Fatalf("the retry did not finish: %q", got)
	}
	if _, err := os.Stat(intentPath(home)); !os.IsNotExist(err) {
		t.Fatalf("an intent is left: %v", err)
	}
}
func TestResolveCodexHome(t *testing.T) {
	base := t.TempDir()
	real, _ := filepath.EvalSymlinks(base)
	if err := os.MkdirAll(filepath.Join(base, "real", "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(base, "real", "sub"), filepath.Join(base, "link")); err != nil {
		t.Fatal(err)
	}
	for in, want := range map[string]string{
		filepath.Join(base, "link") + "/..":   filepath.Join(real, "real"),
		filepath.Join(base, "link"):           filepath.Join(real, "real", "sub"),
		filepath.Join(base, "new", "codex"):   filepath.Join(real, "new", "codex"),
		filepath.Join(base, "link", "fresh"):  filepath.Join(real, "real", "sub", "fresh"),
		filepath.Join(base, "link") + "/./x/": filepath.Join(real, "real", "sub", "x"),
	} {
		if got, err := ResolveCodexHome(in); err != nil || got != want {
			t.Fatalf("ResolveCodexHome(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	if got, err := ResolveCodexHome(filepath.Join(base, "missing") + "/../x"); err == nil {
		t.Fatalf("a '..' after a missing directory was resolved to %q", got)
	}
}
