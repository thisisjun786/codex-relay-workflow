package configguard

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/crwdir"
)

// CRW-877 closes the two gaps CRW-844 and CRW-866 left in the config.toml writer lock. The
// activation's own injected "codex features enable" calls rewrite config.toml and now run inside the
// sidecar lock every CRW writer takes, in the same critical section as the activation's managed-key
// write; the deactivation decides from the install manifest as it is inside the lock, not from the
// copy it read before it started waiting. Each behaviour test drives the real entry point, asserts
// the refusal a held lock produces, and keeps the control that a free lock leaves the command's
// answer exactly as it is today.

// configLockActivationHome is configLockWritersTempHomes plus the isolation the real-state rule asks
// for: HOME, CODEX_HOME and CRW_HOME all live under one temporary root this test owns, so a run of
// this file can never reach the real ~/.codex or ~/.crw.
func configLockActivationHome(t *testing.T) string {
	t.Helper()
	home := configLockWritersTempHomes(t)
	root := os.Getenv("HOME")
	if root == "" {
		t.Fatal("the temporary HOME was not set")
	}
	for key, value := range map[string]string{"CODEX_HOME": os.Getenv("CODEX_HOME"), "CRW_HOME": os.Getenv("CRW_HOME"), "codex home": home} {
		if value == "" || !strings.HasPrefix(value, root+string(os.PathSeparator)) {
			t.Fatalf("%s=%q is not inside the temporary HOME=%q", key, value, root)
		}
	}
	return home
}

// The activation runs "codex features enable" through the injected CLI, which rewrites config.toml
// itself, so those calls are inside the sidecar lock: with another CRW writer holding it the
// activation refuses with the shared busy message and never reaches an enable, and it writes neither
// config.toml nor the install manifest.
func TestActivateHoldsTheConfigLockWhileItEnablesFlags(t *testing.T) {
	home := configLockActivationHome(t)
	path := filepath.Join(home, "config.toml")
	activationWrite(t, path, "[features]\nhooks = false\n")

	held := configLockWritersHold(t, path)
	before := activationRead(t, path)
	// No declared flag is true, so every one of them is an enable this activation would run.
	var calls [][]string
	if _, e := Activate(activationDeps(t, home, map[string]bool{}, &calls)); e == nil || !strings.Contains(e.Error(), crwdir.ConfigLockBusy) {
		t.Fatalf("the activation did not refuse while the lock was held: %v", e)
	}
	for _, call := range calls {
		if len(call) > 1 && call[1] == "enable" {
			t.Fatalf("the refused activation ran features enable: %v", calls)
		}
	}
	if got := activationRead(t, path); got != before {
		t.Fatalf("the refused activation wrote config.toml: %q", got)
	}
	if _, e := os.Stat(manifestPath(home)); !os.IsNotExist(e) {
		t.Fatalf("the refused activation published a manifest: %v", e)
	}
	held.Release()

	// Control: with the lock free the activation answers exactly as today: the same probe and the
	// same enables in declaration order, the same config.toml and the same manifest.
	calls = nil
	m, e := Activate(activationDeps(t, home, map[string]bool{}, &calls))
	if e != nil {
		t.Fatal(e)
	}
	want := [][]string{
		{"features", "list"},
		{"features", "enable", "multi_agent"},
		{"features", "enable", "goals"},
		{"features", "enable", "hooks"},
		{"features", "enable", "default_mode_request_user_input"},
	}
	if !reflect.DeepEqual(calls, want) {
		t.Fatalf("calls = %v, want %v", calls, want)
	}
	if m == nil || !m.Flags["hooks"].EnabledByCodexclaw || !m.TableKeys["memories.dedicated_tools"].SetByCodexclaw {
		t.Fatalf("manifest = %+v", m)
	}
	if got := activationRead(t, path); !strings.Contains(got, "hooks = true") || !strings.Contains(got, "dedicated_tools = true") {
		t.Fatalf("config after the lock was released = %q", got)
	}
	if _, e := os.Stat(path + ".crw-lock"); e != nil {
		t.Fatalf("the sidecar was not left in place: %v", e)
	}
}

// The deactivation decides from the manifest it reads inside the lock. A writer that publishes while
// it waits is the one it follows: the first manifest records this key's prior value as "false", the
// manifest published during the wait records it as absent, and only the second one's restore removes
// the key (failure class 3, the check-then-act race).
func TestDeactivateDecidesFromTheManifestInsideTheLock(t *testing.T) {
	home := configLockActivationHome(t)
	path := filepath.Join(home, "config.toml")
	activationWrite(t, path, deactivationConfig)
	stalePrior := "false"
	deactivationManifest(t, home, map[string]TableKeyRecord{"memories.dedicated_tools": deactivationKey(&stalePrior)}, nil)

	held := configLockWritersHold(t, path)
	done := make(chan *DeactivateResult, 1)
	go func() {
		// The runner here answers only the declared-state probe; this manifest enables no flag, so no
		// disable call is made and the goroutine stays clear of testing.T.
		r, _ := Deactivate(deactivationDeps(home, func([]string) CodexRunResult { return CodexRunResult{} }))
		done <- r
	}()
	time.Sleep(200 * time.Millisecond) // the deactivation is waiting on the lock now
	// While it waits, another writer publishes: this key was absent before the activation.
	deactivationManifest(t, home, map[string]TableKeyRecord{"memories.dedicated_tools": deactivationKey(nil)}, nil)
	held.Release()

	r := <-done
	if r == nil || r.NoManifest || r.FileDrifted || !reflect.DeepEqual(r.RestoredKeys, []string{"memories.dedicated_tools"}) {
		t.Fatalf("result = %+v", r)
	}
	if got := activationRead(t, path); strings.Contains(got, "dedicated_tools") {
		t.Fatalf("the manifest read before the lock decided the restore: %q", got)
	}
}

// Control: with the lock free the deactivation answers exactly as today. The manifest is the same at
// both readings, so the restore writes the recorded prior value back and the opt-out is recorded.
func TestDeactivateWithAFreeConfigLockAnswersAsToday(t *testing.T) {
	home := configLockActivationHome(t)
	path := filepath.Join(home, "config.toml")
	activationWrite(t, path, deactivationConfig)
	prior := "false"
	deactivationManifest(t, home, map[string]TableKeyRecord{"memories.dedicated_tools": deactivationKey(&prior)}, nil)

	r, e := Deactivate(deactivationDeps(home, func([]string) CodexRunResult { return CodexRunResult{} }))
	if e != nil {
		t.Fatal(e)
	}
	if r == nil || r.NoManifest || !reflect.DeepEqual(r.RestoredKeys, []string{"memories.dedicated_tools"}) {
		t.Fatalf("result = %+v", r)
	}
	if got := activationRead(t, path); got != "[memories]\ngenerate_memories = true\ndedicated_tools = false\n" {
		t.Fatalf("restored config = %q", got)
	}
	marker, e := ReadSelfHealMarkerFile(home)
	if e != nil || marker == nil || marker.OptedOut == nil || !*marker.OptedOut {
		t.Fatalf("the deactivation did not record the opt-out: %+v, %v", marker, e)
	}
}

// The second reading is the one the deactivation decides from, so a manifest that disappears while it
// waits must answer as the no-manifest branch does rather than restore from the copy it saw before
// the lock (failure class 8, fail open: an unreadable record must never turn into a restore).
func TestDeactivateAnswersNoManifestWhenTheManifestGoesAwayInsideTheLock(t *testing.T) {
	home := configLockActivationHome(t)
	path := filepath.Join(home, "config.toml")
	activationWrite(t, path, deactivationConfig)
	prior := "false"
	deactivationManifest(t, home, map[string]TableKeyRecord{"memories.dedicated_tools": deactivationKey(&prior)}, nil)

	held := configLockWritersHold(t, path)
	done := make(chan *DeactivateResult, 1)
	go func() {
		r, _ := Deactivate(deactivationDeps(home, func([]string) CodexRunResult { return CodexRunResult{} }))
		done <- r
	}()
	time.Sleep(200 * time.Millisecond) // the deactivation is waiting on the lock now
	if err := os.Remove(manifestPath(home)); err != nil {
		t.Fatal(err)
	}
	held.Release()

	r := <-done
	if r == nil || !r.NoManifest || len(r.RestoredKeys) != 0 || r.FileDrifted {
		t.Fatalf("result = %+v", r)
	}
	if got := activationRead(t, path); got != deactivationConfig {
		t.Fatalf("the deactivation restored from the manifest read before the lock: %q", got)
	}
	if marker, e := ReadSelfHealMarkerFile(home); e != nil || marker == nil || marker.OptedOut == nil || !*marker.OptedOut {
		t.Fatalf("the no-manifest answer did not record the opt-out: %+v, %v", marker, e)
	}
}
