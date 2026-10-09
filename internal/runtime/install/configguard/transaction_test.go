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

// CRW-1153: config.toml, the feature flags and the install manifest that records crw's ownership of them change as one
// recoverable transaction. A failure or a kill at any step leaves either the original state or an exact pending record, and
// the next explicit command records the effects in place, never an unowned change and never a false durable success.

type txKilled struct{ step string }

// txRunStoppedAt runs f with the k-th transaction step (1-based) failed, or the process "killed" there when kill is set:
// the step panics and nothing after it runs, which leaves the files exactly as a killed command would (the only deferred
// work in these commands is the release of the config lock).
func txRunStoppedAt(t *testing.T, k int, kill bool, f func() error) (stopped string, err error) {
	t.Helper()
	n := 0
	txHook = func(step string) error {
		n++
		if n != k {
			return nil
		}
		stopped = step
		if kill {
			panic(txKilled{step})
		}
		return fmt.Errorf("injected failure at %s", step)
	}
	defer func() {
		txHook = nil
		if r := recover(); r != nil {
			if _, ok := r.(txKilled); !ok {
				panic(r)
			}
			err = errors.New("killed at " + stopped)
		}
	}()
	err = f()
	return stopped, err
}

// txCountSteps answers how many transaction steps a clean run of f takes.
func txCountSteps(t *testing.T, f func() error) int {
	t.Helper()
	n := 0
	txHook = func(string) error { n++; return nil }
	defer func() { txHook = nil }()
	if err := f(); err != nil {
		t.Fatal(err)
	}
	return n
}

const txOriginal = "# mine\n[memories]\ngenerate_memories = true\n"

func txActivationFixture(t *testing.T) (string, string, ActivateDeps, map[string]bool) {
	t.Helper()
	home := activationHome(t)
	path := filepath.Join(home, "config.toml")
	activationWrite(t, path, txOriginal)
	state := map[string]bool{}
	var calls [][]string
	deps := activationDeps(t, home, state, &calls)
	base := deps.Run
	deps.Run = func(args []string) CodexRunResult {
		if args[1] == "disable" {
			state[args[2]] = false
			content, _ := os.ReadFile(path)
			activationWrite(t, path, SetTableKey(string(content), "features", args[2], false).Content)
			return CodexRunResult{}
		}
		return base(args)
	}
	return home, path, deps, state
}

// txCheckRecorded asserts the exact pending record: every flag on that was off is crw's, the key crw set is crw's with the
// original value, and no intent is left.
func txCheckRecorded(t *testing.T, home, path string, state map[string]bool) {
	t.Helper()
	if _, err := os.Stat(intentPath(home)); !os.IsNotExist(err) {
		t.Fatalf("an intent is left after the rerun: %v", err)
	}
	m := parseInstallManifest(activationRead(t, manifestPath(home)))
	if m == nil {
		t.Fatal("no manifest after the rerun")
	}
	for _, k := range DeclaredFeatures() {
		if f := m.Flags[string(k)]; state[string(k)] && (!f.EnabledByCodexclaw || f.PriorEnabled) {
			t.Fatalf("flag %s is on but not recorded as crw's: %+v", k, f)
		}
	}
	rec, ok := m.TableKeys["memories.dedicated_tools"]
	if live, _ := semanticRaw(activationRead(t, path), "memories", "dedicated_tools"); live != nil && (!ok || !rec.SetByCodexclaw || rec.PriorValue != nil) {
		t.Fatalf("the key is set but not recorded as crw's: %+v", rec)
	}
}

func TestActivationTransactionSurvivesAFailureOrAKillAtEveryStep(t *testing.T) {
	_, _, clean, _ := txActivationFixture(t)
	steps := txCountSteps(t, func() error { _, err := Activate(clean); return err })
	if steps < 8 {
		t.Fatalf("only %d steps", steps)
	}
	for k := 1; k <= steps; k++ {
		for _, kill := range []bool{false, true} {
			t.Run(fmt.Sprintf("step%d-kill%v", k, kill), func(t *testing.T) {
				home, path, deps, state := txActivationFixture(t)
				stopped, err := txRunStoppedAt(t, k, kill, func() error { _, err := Activate(deps); return err })
				if err == nil {
					t.Fatalf("the activation stopped at %s reported success", stopped)
				}
				if _, statErr := os.Stat(manifestPath(home)); statErr == nil && !kill && stopped != "intent-close" {
					// A failure the activation saw itself either committed the effects so far or changed nothing.
					txCheckRecordedOrPending(t, home, path, state)
				}
				if _, err := Activate(deps); err != nil {
					t.Fatalf("the rerun after a stop at %s: %v", stopped, err)
				}
				txCheckRecorded(t, home, path, state)
				r, err := Deactivate(deactivationDeps(home, deps.Run))
				if err != nil || len(r.Failed) != 0 {
					t.Fatalf("deactivate: %+v %v", r, err)
				}
				for key, on := range state {
					if on {
						t.Fatalf("flag %s is still on after the deactivation (stop at %s)", key, stopped)
					}
				}
				if strings.Contains(activationRead(t, path), "dedicated_tools") {
					t.Fatalf("the key is left after the deactivation (stop at %s): %q", stopped, activationRead(t, path))
				}
			})
		}
	}
}

// txCheckRecordedOrPending accepts a committed manifest that records what is on, or an intent that still holds the change.
func txCheckRecordedOrPending(t *testing.T, home, path string, state map[string]bool) {
	t.Helper()
	if _, err := os.Stat(intentPath(home)); err == nil {
		return
	}
	txCheckRecorded(t, home, path, state)
}

func TestConfigSetTransactionSurvivesAFailureOrAKillAtEveryStep(t *testing.T) {
	cleanHome, cleanPath := configSetHome(t, txOriginal, true)
	value := true
	steps := txCountSteps(t, func() error {
		_, err := ApplyManagedKey(ConfigSetDeps{CodexHome: cleanHome, ConfigPath: cleanPath}, configSetKey, &value)
		return err
	})
	for k := 1; k <= steps; k++ {
		for _, kill := range []bool{false, true} {
			t.Run(fmt.Sprintf("step%d-kill%v", k, kill), func(t *testing.T) {
				home, path := configSetHome(t, txOriginal, true)
				deps := ConfigSetDeps{CodexHome: home, ConfigPath: path}
				stopped, err := txRunStoppedAt(t, k, kill, func() error {
					_, err := ApplyManagedKey(deps, configSetKey, &value)
					return err
				})
				if err == nil {
					t.Fatalf("the set stopped at %s reported success", stopped)
				}
				// The original state, or a change whose intent is pending: never a changed key without one or the other.
				m := configSetManifest(t, home)
				_, pending := os.Stat(intentPath(home))
				if _, recorded := m.TableKeys[configSetKey]; activationRead(t, path) != txOriginal && !recorded && pending != nil {
					t.Fatalf("stop at %s left an unrecorded change: %q", stopped, activationRead(t, path))
				}
				if r, err := ApplyManagedKey(deps, configSetKey, &value); err != nil || !r.OK {
					t.Fatalf("rerun after a stop at %s: %+v %v", stopped, r, err)
				}
				rec := configSetManifest(t, home).TableKeys[configSetKey]
				if !rec.SetByCodexclaw || rec.PriorValue != nil {
					t.Fatalf("after a stop at %s the record is %+v", stopped, rec)
				}
				if r, err := ApplyManagedKey(deps, configSetKey, nil); err != nil || !r.OK || activationRead(t, path) != txOriginal {
					t.Fatalf("unset after a stop at %s: %+v %v %q", stopped, r, err, activationRead(t, path))
				}
				if _, err := os.Stat(intentPath(home)); !os.IsNotExist(err) {
					t.Fatalf("an intent is left: %v", err)
				}
			})
		}
	}
}

func TestConfigSetWithAnUnwritableManifestChangesNothing(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root writes a mode-0444 file")
	}
	home, path := configSetHome(t, txOriginal, true)
	if err := os.Chmod(manifestPath(home), 0o444); err != nil {
		t.Fatal(err)
	}
	manifest := activationRead(t, manifestPath(home))
	value := true
	r, err := ApplyManagedKey(ConfigSetDeps{CodexHome: home, ConfigPath: path}, configSetKey, &value)
	if err == nil || r.OK || activationRead(t, path) != txOriginal || activationRead(t, manifestPath(home)) != manifest {
		t.Fatalf("an unwritable manifest: %+v %v, config %q", r, err, activationRead(t, path))
	}
	if backups, _ := filepath.Glob(path + ".crw-*.bak"); len(backups) != 0 {
		t.Fatalf("a refused set left backups: %v", backups)
	}
	if _, err := os.Stat(intentPath(home)); !os.IsNotExist(err) {
		t.Fatalf("a refused set left an intent: %v", err)
	}
}

func TestActivateHardFailureRecordsTheFlagsItEnabled(t *testing.T) {
	home, path, deps, state := txActivationFixture(t)
	base := deps.Run
	deps.Run = func(args []string) CodexRunResult {
		if args[1] == "enable" && args[2] == "goals" {
			return CodexRunResult{ExitCode: 2, Stderr: "goals is not available"}
		}
		return base(args)
	}
	m, err := Activate(deps)
	if err == nil || m != nil || !strings.Contains(err.Error(), "codex features enable goals failed") {
		t.Fatalf("%+v %v", m, err)
	}
	if !state["multi_agent"] {
		t.Fatal("the fixture did not enable multi_agent first")
	}
	txCheckRecorded(t, home, path, state)
	if _, err := Deactivate(deactivationDeps(home, deps.Run)); err != nil || state["multi_agent"] {
		t.Fatalf("the deactivation did not revert the flag the failed activation enabled: %v %v", state, err)
	}
}

func TestActivateRefusesAMalformedManifest(t *testing.T) {
	home, path, deps, state := txActivationFixture(t)
	activationWrite(t, manifestPath(home), "truncated {")
	m, err := Activate(deps)
	if err == nil || m != nil || !strings.Contains(err.Error(), "not one crw can read") {
		t.Fatalf("%+v %v", m, err)
	}
	if activationRead(t, manifestPath(home)) != "truncated {" || activationRead(t, path) != txOriginal || len(state) != 0 {
		t.Fatal("a malformed manifest was replaced, or config.toml or a flag changed")
	}
}

func TestTransactionReportsAnUnsyncedDirectoryWithTheRecordInPlace(t *testing.T) {
	saved := activationCrwdirPublish
	t.Cleanup(func() { activationCrwdirPublish = saved })
	home, path, deps, state := txActivationFixture(t)
	activationCrwdirPublish = func(p string, b []byte) error {
		if err := saved(p, b); err != nil {
			return err
		}
		if p == manifestPath(home) {
			return &crwdir.PublishedError{Err: errors.New("injected directory sync failure")}
		}
		return nil
	}
	m, err := Activate(deps)
	if m == nil || err == nil || !crwdir.Published(err) || !strings.Contains(err.Error(), "may not survive a power failure") {
		t.Fatalf("an unsynced manifest was reported as durable: %+v %v", m, err)
	}
	txCheckRecorded(t, home, path, state)
	cfgHome, cfgPath := configSetHome(t, txOriginal, true)
	activationCrwdirPublish = func(p string, b []byte) error {
		if err := saved(p, b); err != nil {
			return err
		}
		if p == cfgPath {
			return &crwdir.PublishedError{Err: errors.New("injected directory sync failure")}
		}
		return nil
	}
	value := true
	r, err := ApplyManagedKey(ConfigSetDeps{CodexHome: cfgHome, ConfigPath: cfgPath}, configSetKey, &value)
	if !r.OK || err == nil || !crwdir.Published(err) || !configSetManifest(t, cfgHome).TableKeys[configSetKey].SetByCodexclaw {
		t.Fatalf("an unsynced config.toml lost its record or was reported durable: %+v %v", r, err)
	}
}

// The control for recovery: a user who changes the key after an interrupted set keeps the change, and the recovery does not
// adopt it as crw's.
func TestRecoveryNeverAdoptsOrRevertsAConcurrentExternalEdit(t *testing.T) {
	home, path := configSetHome(t, txOriginal, true)
	deps := ConfigSetDeps{CodexHome: home, ConfigPath: path}
	value := true
	stopped, err := txRunStoppedAt(t, 4, true, func() error { _, err := ApplyManagedKey(deps, configSetKey, &value); return err })
	if err == nil || stopped != "manifest" {
		t.Fatalf("stopped at %q: %v", stopped, err)
	}
	if !strings.Contains(activationRead(t, path), "dedicated_tools = true") {
		t.Fatalf("the set did not reach config.toml before the kill: %q", activationRead(t, path))
	}
	edited := strings.Replace(activationRead(t, path), "dedicated_tools = true", "dedicated_tools = false", 1)
	activationWrite(t, path, edited)
	r, err := ApplyManagedKey(deps, configSetKey, nil)
	if err != nil || r.OK || !strings.Contains(r.Reason, "is not recorded") || activationRead(t, path) != edited {
		t.Fatalf("recovery adopted or reverted the user's edit: %+v %v %q", r, err, activationRead(t, path))
	}
	if _, err := os.Stat(intentPath(home)); !os.IsNotExist(err) {
		t.Fatalf("the recovered intent is left: %v", err)
	}
}
