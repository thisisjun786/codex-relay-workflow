package configguard

import (
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"golang.org/x/sys/unix"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/crwdir"
)

// CRW-877 closes the two gaps CRW-844 and CRW-866 left in the config.toml writer lock. The
// activation's own injected "codex features enable" calls rewrite config.toml and now run inside the
// sidecar lock every CRW writer takes, in the same critical section as the activation's managed-key
// write and its declared-state probe; the deactivation decides from the install manifest as it is
// inside the lock, not from the copy it read before it started waiting. Each behaviour test drives
// the real entry point, asserts the refusal a held lock produces, and keeps the control that a free
// lock leaves the command's answer exactly as it is today.
//
// The interleaving tests rendezvous instead of sleeping: the manifest path is a FIFO for the first
// reading, so the deactivation blocks there until the test hands it the manifest that reading must
// see, and everything the test does after that write is the concurrent writer's publication.

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

// configLockActivationManifestBytes is the on-disk form of one manifest the tests hand over.
func configLockActivationManifestBytes(t *testing.T, m *InstallManifest) []byte {
	t.Helper()
	b, err := manifestBytes(m)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// configLockActivationHandover turns the manifest path into a FIFO and starts the writer goroutine.
// The goroutine's open for writing blocks until the deactivation opens the manifest for reading, so
// the bytes it then writes are exactly the first reading's content and everything after that write
// (publish, then release) happens while the deactivation is waiting for the lock. No sleep guesses
// when the deactivation reached the read.
func configLockActivationHandover(t *testing.T, home string, stale []byte, publish func() error, release func()) {
	t.Helper()
	fifo := manifestPath(home)
	if err := unix.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	go func() {
		f, err := os.OpenFile(fifo, os.O_WRONLY, 0)
		if err != nil {
			t.Error(err)
			return
		}
		if _, err := f.Write(stale); err != nil {
			t.Error(err)
		}
		if err := f.Close(); err != nil {
			t.Error(err)
		}
		// The FIFO has served its purpose: the second reading must see what publish leaves behind.
		if err := os.Remove(fifo); err != nil {
			t.Error(err)
			return
		}
		if err := publish(); err != nil {
			t.Error(err)
		}
		release()
	}()
}

// The activation runs "codex features enable" through the injected CLI, which rewrites config.toml
// itself, so those calls are inside the sidecar lock. Its declared-state probe reads the same file
// through that CLI, so the probe is inside the lock too: with another CRW writer holding it the
// activation refuses with the shared busy message and never reaches the runner at all, and it writes
// neither config.toml nor the install manifest.
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
	if len(calls) != 0 {
		t.Fatalf("the refused activation reached the runner: %v", calls)
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
	stale := configLockActivationManifestBytes(t, &InstallManifest{Version: 2, ConfigPath: path, Flags: map[string]FlagRecord{}, TableKeys: map[string]TableKeyRecord{"memories.dedicated_tools": deactivationKey(&stalePrior)}})
	fresh := configLockActivationManifestBytes(t, &InstallManifest{Version: 2, ConfigPath: path, Flags: map[string]FlagRecord{}, TableKeys: map[string]TableKeyRecord{"memories.dedicated_tools": deactivationKey(nil)}})

	held := configLockWritersHold(t, path)
	configLockActivationHandover(t, home, stale, func() error {
		// While it waits, another writer publishes: this key was absent before the activation.
		return os.WriteFile(manifestPath(home), fresh, 0o644)
	}, held.Release)

	r, err := Deactivate(deactivationDeps(home, func([]string) CodexRunResult { return CodexRunResult{} }))
	if err != nil {
		t.Fatal(err)
	}
	if r == nil || r.NoManifest || r.FileDrifted || !reflect.DeepEqual(r.RestoredKeys, []string{"memories.dedicated_tools"}) {
		t.Fatalf("result = %+v", r)
	}
	if got := activationRead(t, path); strings.Contains(got, "dedicated_tools") {
		t.Fatalf("the manifest read before the lock decided the restore: %q", got)
	}
}

// The same defect through the shape the operator's post-merge evaluation named: config.toml holds
// the managed key, writer A runs "config set memories.dedicated_tools false" and holds the lock, and
// the deactivation reads the old manifest before that. A publishes false with applied=false and
// releases; the deactivation then takes the lock and must decide from the manifest it re-reads
// there. Deciding from the old one would compare the live false with the stale applied=true, report
// the key as changed by someone else and leave the managed key behind.
func TestDeactivateFollowsAManifestRepublishedWhileItWaits(t *testing.T) {
	home := configLockActivationHome(t)
	path := filepath.Join(home, "config.toml")
	activationWrite(t, path, deactivationConfig)
	stale := configLockActivationManifestBytes(t, &InstallManifest{Version: 2, ConfigPath: path, Flags: map[string]FlagRecord{}, TableKeys: map[string]TableKeyRecord{"memories.dedicated_tools": deactivationKey(nil)}})
	freshManifest := &InstallManifest{Version: 2, ConfigPath: path, Flags: map[string]FlagRecord{}, TableKeys: map[string]TableKeyRecord{"memories.dedicated_tools": {"memories", "dedicated_tools", nil, "false", true}}}
	fresh := configLockActivationManifestBytes(t, freshManifest)

	held := configLockWritersHold(t, path)
	configLockActivationHandover(t, home, stale, func() error {
		// What config set leaves behind before it republishes the manifest: the key is false now and
		// the manifest records that value as the one CRW applied.
		activationWrite(t, path, "[memories]\ngenerate_memories = true\ndedicated_tools = false\n")
		freshManifest.PostActivateHash, _ = hashOrNull(path)
		fresh = configLockActivationManifestBytes(t, freshManifest)
		return os.WriteFile(manifestPath(home), fresh, 0o644)
	}, held.Release)

	r, err := Deactivate(deactivationDeps(home, func([]string) CodexRunResult { return CodexRunResult{} }))
	if err != nil {
		t.Fatal(err)
	}
	if r == nil || !slices.Contains(r.RestoredKeys, "memories.dedicated_tools") {
		t.Fatalf("the deactivation did not restore the key from the manifest it re-read: %+v", r)
	}
	for _, skipped := range r.SkippedExternal {
		if skipped.Target == "memories.dedicated_tools" {
			t.Fatalf("the deactivation reported CRW's own key as someone else's change: %+v", r)
		}
	}
	if got := activationRead(t, path); strings.Contains(got, "dedicated_tools") {
		t.Fatalf("the deactivation left the managed key behind: %q", got)
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
	stale := configLockActivationManifestBytes(t, &InstallManifest{Version: 2, ConfigPath: path, Flags: map[string]FlagRecord{}, TableKeys: map[string]TableKeyRecord{"memories.dedicated_tools": deactivationKey(&prior)}})

	held := configLockWritersHold(t, path)
	// The handover already removed the FIFO, so the path is absent when the second reading runs.
	configLockActivationHandover(t, home, stale, func() error { return nil }, held.Release)

	r, err := Deactivate(deactivationDeps(home, func([]string) CodexRunResult { return CodexRunResult{} }))
	if err != nil {
		t.Fatal(err)
	}
	if r == nil || !r.NoManifest || len(r.RestoredKeys) != 0 || r.FileDrifted {
		t.Fatalf("result = %+v", r)
	}
	if got := activationRead(t, path); got != deactivationConfig {
		t.Fatalf("the deactivation restored from the manifest read before the lock: %q", got)
	}
	marker, e := ReadSelfHealMarkerFile(home)
	if e != nil || marker == nil || marker.OptedOut == nil || !*marker.OptedOut {
		t.Fatalf("the no-manifest answer did not record the opt-out: %+v, %v", marker, e)
	}
}

// A manifest that starts naming a different config file while the deactivation waits would have it
// apply one file's ownership records to another under a lock taken on the old file, so it refuses
// instead (fail closed) and writes nothing. An explicit ConfigPath overrides the manifest in both
// readings, so only the derived path can disagree.
func TestDeactivateRefusesAManifestThatNamesADifferentConfigFile(t *testing.T) {
	home := configLockActivationHome(t)
	path := filepath.Join(home, "config.toml")
	activationWrite(t, path, deactivationConfig)
	stale := configLockActivationManifestBytes(t, &InstallManifest{Version: 2, ConfigPath: path, Flags: map[string]FlagRecord{}, TableKeys: map[string]TableKeyRecord{"memories.dedicated_tools": deactivationKey(nil)}})
	moved := &InstallManifest{Version: 2, ConfigPath: filepath.Join(home, "other.toml"), Flags: map[string]FlagRecord{}, TableKeys: map[string]TableKeyRecord{"memories.dedicated_tools": deactivationKey(nil)}}
	fresh := configLockActivationManifestBytes(t, moved)

	held := configLockWritersHold(t, path)
	configLockActivationHandover(t, home, stale, func() error {
		return os.WriteFile(manifestPath(home), fresh, 0o644)
	}, held.Release)

	_, err := Deactivate(deactivationDeps(home, func([]string) CodexRunResult { return CodexRunResult{} }))
	if err == nil || !strings.Contains(err.Error(), "names a different config file") {
		t.Fatalf("the deactivation did not refuse the moved manifest: %v", err)
	}
	if got := activationRead(t, path); got != deactivationConfig {
		t.Fatalf("the refused deactivation wrote: %q", got)
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
