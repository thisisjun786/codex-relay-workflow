package configguard

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/crwdir"
)

// The first verification round of this lane (CRW-1141, CRW-1143, CRW-1144, CRW-1145, CRW-1153) reproduced eight defects with
// counterexamples; each test below is one of them, written to fail on the code that was verified and to pass on the fix.

// failIntentSync makes the n-th (1-based) publication of the install intent report a directory-sync failure after the file is
// in place, and counts the publications. It answers the counter.
func failIntentSync(t *testing.T, home string, n int) *int {
	t.Helper()
	saved := activationCrwdirPublish
	t.Cleanup(func() { activationCrwdirPublish = saved })
	count := new(int)
	activationCrwdirPublish = func(p string, b []byte) error {
		if err := saved(p, b); err != nil {
			return err
		}
		if p == intentPath(home) {
			*count++
			if *count == n {
				return &crwdir.PublishedError{Err: errors.New("intent directory sync failed")}
			}
		}
		return nil
	}
	return count
}

// CRW-1153: an effect runs only on an intent known to be durable. A directory-sync failure of any intent publication (the
// first, or the one that marks an effect attempted) stops the activation before that effect and keeps the ownership of the
// effects already in place.
func TestIntentThatMayNotBeDurableStopsBeforeEveryEffect(t *testing.T) {
	home, _, deps, _ := txActivationFixture(t)
	published := failIntentSync(t, home, 0)
	if _, err := Activate(deps); err != nil {
		t.Fatal(err)
	}
	total := *published
	if total != 8 { // the first intent, one per flag, the last flag's done record, the managed key's attempt and its done record
		t.Fatalf("a clean activation publishes %d intents", total)
	}
	for n := 1; n <= total; n++ {
		t.Run(fmt.Sprintf("intent%d", n), func(t *testing.T) {
			home, path, deps, state := txActivationFixture(t)
			failIntentSync(t, home, n)
			enables := 0
			base := deps.Run
			deps.Run = func(args []string) CodexRunResult {
				if args[1] == "enable" {
					enables++
				}
				return base(args)
			}
			_, err := Activate(deps)
			if n == total {
				// The last intent is the done record of the key, published after the key is in place: the command has done and
				// recorded everything, and reports that the record may not be durable.
				if err == nil || !crwdir.Published(err) || !strings.Contains(err.Error(), "may not survive a power failure") {
					t.Fatalf("an unsynced done record was not reported as a change in place: %v", err)
				}
				txCheckRecorded(t, home, path, state)
				return
			}
			if err == nil || !strings.Contains(err.Error(), "may not survive a power failure") || crwdir.Published(err) {
				t.Fatalf("an intent that may not be durable did not stop the activation: %v", err)
			}
			wantEnables := max(min(n-2, 4), 0)
			if enables != wantEnables {
				t.Fatalf("%d flag effects ran after the intent %d failed, want %d", enables, n, wantEnables)
			}
			if strings.Contains(activationRead(t, path), "dedicated_tools") {
				t.Fatalf("the managed key was written before its intent was durable: %q", activationRead(t, path))
			}
			if n == 1 {
				if _, err := os.Stat(intentPath(home)); !os.IsNotExist(err) {
					t.Fatalf("an intent no effect depends on is left: %v", err)
				}
				if _, err := os.Stat(manifestPath(home)); !os.IsNotExist(err) {
					t.Fatalf("a manifest was written for nothing: %v", err)
				}
			} else {
				txCheckRecorded(t, home, path, state)
			}
			// The rerun finishes the activation, and the deactivation reverts all of it.
			activationCrwdirPublish = crwdir.Publish
			if _, err := Activate(deps); err != nil {
				t.Fatalf("rerun: %v", err)
			}
			txCheckRecorded(t, home, path, state)
			if r, err := Deactivate(deactivationDeps(home, deps.Run)); err != nil || len(r.Failed) != 0 {
				t.Fatalf("deactivate: %+v %v", r, err)
			}
			for key, on := range state {
				if on {
					t.Fatalf("flag %s is still on", key)
				}
			}
		})
	}
}

// CRW-1153: config set writes config.toml only on a durable intent.
func TestConfigSetIntentThatMayNotBeDurableChangesNothing(t *testing.T) {
	home, path := configSetHome(t, txOriginal, true)
	failIntentSync(t, home, 1)
	value := true
	r, err := ApplyManagedKey(ConfigSetDeps{CodexHome: home, ConfigPath: path}, configSetKey, &value)
	if err == nil || r.OK || activationRead(t, path) != txOriginal {
		t.Fatalf("config.toml changed on an intent that may not be durable: %+v %v %q", r, err, activationRead(t, path))
	}
	if _, err := os.Stat(intentPath(home)); !os.IsNotExist(err) {
		t.Fatalf("an intent no effect depends on is left: %v", err)
	}
}

// CRW-1143, CRW-1153: a runner that exits nonzero after it wrote the flag changed the flag. The flag is crw's, so the
// deactivation reverts it, and the failure is still reported.
func TestNonzeroRunnerThatAppliedTheFlagKeepsOwnership(t *testing.T) {
	home, path, deps, state := txActivationFixture(t)
	base := deps.Run
	deps.Run = func(args []string) CodexRunResult {
		r := base(args)
		if args[1] == "enable" && args[2] == "goals" {
			r.ExitCode, r.Stderr = 2, "failed after the write"
		}
		return r
	}
	if _, err := Activate(deps); err == nil || !strings.Contains(err.Error(), "enable goals failed") {
		t.Fatalf("the failure is not reported: %v", err)
	}
	if !state["goals"] {
		t.Fatal("the fixture did not apply the flag")
	}
	m := parseInstallManifest(activationRead(t, manifestPath(home)))
	goals := m.Flags["goals"]
	if !goals.EnabledByCodexclaw || !goals.EnableFailed || goals.Failure == nil || goals.Failure.ExitCode != 2 {
		t.Fatalf("an applied flag lost its ownership, or the failure its record: %+v", goals)
	}
	if _, err := os.Stat(intentPath(home)); !os.IsNotExist(err) {
		t.Fatalf("an intent is left: %v", err)
	}
	txCheckRecorded(t, home, path, state)
	if r, err := Deactivate(deactivationDeps(home, base)); err != nil || len(r.Failed) != 0 || state["goals"] || state["multi_agent"] {
		t.Fatalf("the deactivation left a flag crw turned on: %+v %v %v", r, err, state)
	}
}

// CRW-1144: when the first feature run replaces config.toml and another writer takes the new file's lock, no later feature run
// goes ahead under the lock of the file that was left; the intent stays, and the retry records everything.
func TestLockIsCheckedAfterEveryRunnerBeforeTheNextEffect(t *testing.T) {
	home := configLockActivationHome(t)
	path := filepath.Join(home, "config.toml")
	target := filepath.Join(home, "target.toml")
	activationWrite(t, target, configLockPathsPre)
	if err := os.Symlink("target.toml", path); err != nil {
		t.Fatal(err)
	}
	var calls [][]string
	base := configLockPathsRenameRunner(t, path, &calls)
	var other *crwdir.ConfigLock
	var writes []string
	deps := ActivateDeps{CodexHome: home, Now: func() string { return "2026-06-30T00:00:00.000Z" }, Run: func(args []string) CodexRunResult {
		if args[1] == "enable" && other != nil {
			writes = append(writes, args[2])
		}
		r := base(args)
		if args[1] == "enable" && other == nil {
			var err error
			if other, err = crwdir.LockConfig(path, 0); err != nil {
				t.Fatal(err)
			}
		}
		return r
	}}
	_, err := Activate(deps)
	if other == nil {
		t.Fatal("the runner did not replace config.toml")
	}
	other.Release()
	if err == nil || len(writes) != 0 {
		t.Fatalf("feature runs went ahead without the new file's lock: %v, err %v", writes, err)
	}
	if _, statErr := os.Stat(intentPath(home)); statErr != nil {
		t.Fatalf("the pending record is gone: %v", statErr)
	}
	if _, err := Activate(ActivateDeps{CodexHome: home, Run: base, Now: deps.Now}); err != nil {
		t.Fatalf("retry: %v", err)
	}
	m := parseInstallManifest(activationRead(t, manifestPath(home)))
	if !m.Flags["multi_agent"].EnabledByCodexclaw || !strings.Contains(activationRead(t, path), "dedicated_tools = true") {
		t.Fatalf("the retry did not record and finish: %+v", m.Flags)
	}
}

// CRW-1153: a stop before an effect runs commits the ownership the install already had.
func TestStopBeforeAnEffectKeepsTheOwnershipItStartedFrom(t *testing.T) {
	for _, stop := range []string{"hard flag failure", "key publication"} {
		t.Run(stop, func(t *testing.T) {
			home, path, deps, state := txActivationFixture(t)
			if _, err := Activate(deps); err != nil {
				t.Fatal(err)
			}
			off := false
			if r, err := ApplyManagedKey(ConfigSetDeps{CodexHome: home, ConfigPath: path}, configSetKey, &off); err != nil || !r.OK {
				t.Fatalf("set: %+v %v", r, err)
			}
			want := parseInstallManifest(activationRead(t, manifestPath(home))).TableKeys[configSetKey]
			if !want.SetByCodexclaw || want.PriorValue != nil {
				t.Fatalf("the install does not own the key to begin with: %+v", want)
			}
			if stop == "hard flag failure" {
				state["goals"] = false
				base := deps.Run
				deps.Run = func(args []string) CodexRunResult {
					if args[1] == "enable" && args[2] == "goals" {
						return CodexRunResult{ExitCode: 2, Stderr: "not available"}
					}
					return base(args)
				}
			} else {
				txHook = func(step string) error {
					if step == "config" {
						return errors.New("injected key publication failure")
					}
					return nil
				}
				t.Cleanup(func() { txHook = nil })
			}
			if _, err := Activate(deps); err == nil {
				t.Fatal("the stop was not reported")
			}
			got := parseInstallManifest(activationRead(t, manifestPath(home))).TableKeys[configSetKey]
			if got != want {
				t.Fatalf("the stop discarded the record: %+v, was %+v", got, want)
			}
		})
	}
}

// CRW-1145: a key set into an install a deactivation released has no live record to belong to. It is refused, and
// 'features enable' starts the new baseline in which the key is owned and the next disable restores it.
func TestConfigSetAfterTheInstallWasReleased(t *testing.T) {
	home, path, deps, _ := txActivationFixture(t)
	if _, err := Activate(deps); err != nil {
		t.Fatal(err)
	}
	if _, err := Deactivate(deactivationDeps(home, deps.Run)); err != nil {
		t.Fatal(err)
	}
	released := activationRead(t, path)
	value := true
	r, err := ApplyManagedKey(ConfigSetDeps{CodexHome: home, ConfigPath: path}, configSetKey, &value)
	if err != nil || r.OK || !strings.Contains(r.Reason, "features enable") || activationRead(t, path) != released {
		t.Fatalf("a set into a released install: %+v %v", r, err)
	}
	if _, err := Activate(deps); err != nil {
		t.Fatal(err)
	}
	if r, err := ApplyManagedKey(ConfigSetDeps{CodexHome: home, ConfigPath: path}, configSetKey, &value); err != nil || !r.OK {
		t.Fatalf("set: %+v %v", r, err)
	}
	d, err := Deactivate(deactivationDeps(home, deps.Run))
	if err != nil || d.Released == false && len(d.RestoredKeys) == 0 || strings.Contains(activationRead(t, path), "dedicated_tools = true") {
		t.Fatalf("the new key was not restored: %+v %v %q", d, err, activationRead(t, path))
	}
}

func TestDeactivateReportsAnUnsyncedReleaseAndRecovery(t *testing.T) {
	saved := activationCrwdirPublish
	t.Cleanup(func() { activationCrwdirPublish = saved })
	t.Run("release", func(t *testing.T) {
		home, _, deps, state := txActivationFixture(t)
		if _, err := Activate(deps); err != nil {
			t.Fatal(err)
		}
		activationCrwdirPublish = func(p string, b []byte) error {
			if err := saved(p, b); err != nil {
				return err
			}
			if p == manifestPath(home) {
				return &crwdir.PublishedError{Err: errors.New("injected directory sync")}
			}
			return nil
		}
		r, err := Deactivate(deactivationDeps(home, deps.Run))
		if err == nil || !crwdir.Published(err) || r == nil || len(r.Disabled) != 4 || state["goals"] {
			t.Fatalf("an unsynced release was reported as success: %+v %v", r, err)
		}
		if m := parseInstallManifest(activationRead(t, manifestPath(home))); m.ReleasedAt == nil {
			t.Fatal("the release is not in place")
		}
	})
	t.Run("recovery", func(t *testing.T) {
		activationCrwdirPublish = saved
		home, _, deps, state := txActivationFixture(t)
		var steps []string
		txHook = func(step string) error {
			steps = append(steps, step)
			if step == "manifest" {
				panic(txKilled{step})
			}
			return nil
		}
		func() {
			defer func() {
				txHook = nil
				if r := recover(); r != nil {
					if _, ok := r.(txKilled); !ok {
						panic(r)
					}
				}
			}()
			_, _ = Activate(deps)
		}()
		if _, err := os.Stat(intentPath(home)); err != nil {
			t.Fatalf("no pending intent: %v", err)
		}
		activationCrwdirPublish = func(p string, b []byte) error {
			if err := saved(p, b); err != nil {
				return err
			}
			if p == manifestPath(home) {
				return &crwdir.PublishedError{Err: errors.New("injected directory sync")}
			}
			return nil
		}
		r, err := Deactivate(deactivationDeps(home, deps.Run))
		if err == nil || !crwdir.Published(err) || r == nil || len(r.Recovered) == 0 {
			t.Fatalf("an unsynced recovery was reported as success: %+v %v", r, err)
		}
		for key, on := range state {
			if on {
				t.Fatalf("flag %s was not reverted", key)
			}
		}
	})
}
