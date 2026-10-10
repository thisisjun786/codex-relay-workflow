package configguard

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The third verification round of this lane, the activation and the recovery: an activation that stops before it has
// observed a flag, and a recovery that takes an attempted marker for proof that crw made the change. Each test fails on the
// code that was verified (c3081dc3) and passes on the fix.

// r3KillAtStep runs f and "kills" the process at the first transaction step named step: nothing after it runs.
func r3KillAtStep(t *testing.T, step string, f func() error) (killed bool) {
	t.Helper()
	txHook = func(s string) error {
		if s == step && !killed {
			killed = true
			panic(txKilled{s})
		}
		return nil
	}
	defer func() {
		txHook = nil
		if r := recover(); r != nil {
			if _, ok := r.(txKilled); !ok {
				panic(r)
			}
		}
	}()
	_ = f()
	return killed
}

// CRW-1143, CRW-1144, CRW-1145: a flag whose enable exited 0 without changing it is not crw's when the next attempt marker
// cannot be published. The stop reads the flags back before it commits any ownership.
func TestAttemptMarkerFailureAfterANoopEnableCommitsNoOwnership(t *testing.T) {
	home, _, deps, state := txActivationFixture(t)
	base := deps.Run
	deps.Run = func(args []string) CodexRunResult {
		if args[1] == "enable" && args[2] == "multi_agent" {
			return CodexRunResult{}
		}
		return base(args)
	}
	n := 0
	txHook = func(step string) error {
		if step == "intent" {
			n++
			if n == 3 { // the first intent, the multi_agent attempt, the goals attempt
				return os.ErrPermission
			}
		}
		return nil
	}
	t.Cleanup(func() { txHook = nil })
	_, err := Activate(deps)
	txHook = nil
	if err == nil || state["multi_agent"] {
		t.Fatalf("fixture: %v %v", err, state)
	}
	m := parseInstallManifest(activationRead(t, manifestPath(home)))
	if m == nil || m.Flags["multi_agent"].EnabledByCodexclaw {
		t.Fatalf("an enable that changed nothing was committed as crw's: %+v", m)
	}
	// The user enables it afterwards: the deactivation leaves the user's flag on.
	state["multi_agent"] = true
	if _, err := Deactivate(deactivationDeps(home, deps.Run)); err != nil || !state["multi_agent"] {
		t.Fatalf("the deactivation turned off a flag crw never enabled: %v %v", err, state)
	}
}

// CRW-1141, CRW-1145, CRW-1149, CRW-1153: a stop between the attempt marker and the effect, followed by the user making the
// planned change, is not adopted as crw's. The flag runner panics (the process is killed) before it enables goals.
func TestKilledBeforeAFlagRunsIsNotAdoptedWhenTheUserEnablesIt(t *testing.T) {
	home, path, deps, state := txActivationFixture(t)
	base := deps.Run
	killing := true
	deps.Run = func(args []string) CodexRunResult {
		if killing && args[1] == "enable" && args[2] == "goals" {
			panic(txKilled{"runner"})
		}
		return base(args)
	}
	func() {
		defer func() {
			if r := recover(); r == nil {
				t.Fatal("the runner did not stop the command")
			}
		}()
		_, _ = Activate(deps)
	}()
	killing = false
	if !state["multi_agent"] || state["goals"] {
		t.Fatalf("fixture: %v", state)
	}
	state["goals"] = true // the user enables it by hand
	activationWrite(t, path, SetTableKey(activationRead(t, path), "features", "goals", true).Content)
	r, err := Deactivate(deactivationDeps(home, deps.Run))
	if err != nil {
		t.Fatal(err)
	}
	if !state["goals"] {
		t.Fatalf("the deactivation turned off a flag the user enabled after the interrupted command: %+v", r)
	}
	if state["multi_agent"] {
		t.Fatalf("the flag crw did enable before the stop lost its ownership: %+v", r)
	}
}

// CRW-1153: the same for a managed key. The command stops at the config publication; the user then writes the value crw
// planned. Neither the recovery nor the unset adopts it.
func TestKilledBeforeTheKeyIsWrittenIsNotAdoptedWhenTheUserSetsIt(t *testing.T) {
	home, path := configSetHome(t, txOriginal, true)
	deps := ConfigSetDeps{CodexHome: home, ConfigPath: path}
	value := true
	if !r3KillAtStep(t, "config", func() error { _, err := ApplyManagedKey(deps, configSetKey, &value); return err }) {
		t.Fatal("the config step was not reached")
	}
	if activationRead(t, path) != txOriginal {
		t.Fatal("fixture: the key reached config.toml before the kill")
	}
	edited := txOriginal + "dedicated_tools = true\n"
	activationWrite(t, path, edited)
	r, err := ApplyManagedKey(deps, configSetKey, nil)
	if err != nil || r.OK || activationRead(t, path) != edited {
		t.Fatalf("the unset removed a key the user wrote: %+v %v %q", r, err, activationRead(t, path))
	}
	if _, ok := configSetManifest(t, home).TableKeys[configSetKey]; ok {
		t.Fatal("the user's key was recorded as crw's")
	}
}

// CRW-1153: the activation's managed key, killed before it is written and set by the user, is left by the deactivation.
func TestActivationKeyKilledBeforeItsWriteIsNotAdopted(t *testing.T) {
	home, path, deps, _ := txActivationFixture(t)
	if !r3KillAtStep(t, "config", func() error { _, err := Activate(deps); return err }) {
		t.Fatal("the config step was not reached")
	}
	content := activationRead(t, path)
	if strings.Contains(content, "dedicated_tools") {
		t.Fatal("fixture: the key reached config.toml before the kill")
	}
	content = strings.Replace(content, "generate_memories = true\n", "generate_memories = true\ndedicated_tools = true\n", 1)
	activationWrite(t, path, content)
	if _, err := Deactivate(deactivationDeps(home, deps.Run)); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(activationRead(t, path), "dedicated_tools = true") {
		t.Fatalf("the deactivation removed a key the user wrote: %q", activationRead(t, path))
	}
}

// CRW-1153: a config.toml retargeted to another file after an interrupted set is not the file the intent is about; the
// recovery does not carry ownership to it, and the unset leaves it alone.
func TestRecoveryDoesNotCarryOwnershipToARetargetedConfig(t *testing.T) {
	home, path := configSetHome(t, txOriginal, true)
	a, b := filepath.Join(home, "a.toml"), filepath.Join(home, "b.toml")
	activationWrite(t, a, txOriginal)
	activationWrite(t, b, txOriginal+"dedicated_tools = true\n")
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("a.toml", path); err != nil {
		t.Fatal(err)
	}
	deps := ConfigSetDeps{CodexHome: home, ConfigPath: path}
	value := true
	if !r3KillAtStep(t, "manifest", func() error { _, err := ApplyManagedKey(deps, configSetKey, &value); return err }) {
		t.Fatal("the manifest step was not reached")
	}
	if !strings.Contains(activationRead(t, a), "dedicated_tools = true") {
		t.Fatal("fixture: the set did not reach a.toml")
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("b.toml", path); err != nil {
		t.Fatal(err)
	}
	r, err := ApplyManagedKey(deps, configSetKey, nil)
	if r.OK || activationRead(t, b) != txOriginal+"dedicated_tools = true\n" {
		t.Fatalf("the unset changed the file the config now names: %+v %v %q", r, err, activationRead(t, b))
	}
}

// CRW-1153: an unreadable manifest is refused by the recovery too, and the intent is kept.
func TestRecoveryRefusesAMalformedManifest(t *testing.T) {
	home, _, deps, _ := txActivationFixture(t)
	if !r3KillAtStep(t, "manifest", func() error { _, err := Activate(deps); return err }) {
		t.Fatal("the manifest step was not reached")
	}
	activationWrite(t, manifestPath(home), "truncated {")
	m, err := Activate(deps)
	if err == nil || m != nil || !strings.Contains(err.Error(), "not one crw can read") {
		t.Fatalf("%+v %v", m, err)
	}
	if activationRead(t, manifestPath(home)) != "truncated {" {
		t.Fatal("the recovery replaced the malformed manifest")
	}
	if _, err := os.Stat(intentPath(home)); err != nil {
		t.Fatalf("the intent was removed: %v", err)
	}
}

// CRW-1141: a config.toml that does not decode is refused by the recovery before it commits anything, and the intent is
// kept; once the file reads again the recovery records the key.
func TestRecoveryOnAnInvalidConfigKeepsTheIntent(t *testing.T) {
	home, path := configSetHome(t, txOriginal, true)
	deps := ConfigSetDeps{CodexHome: home, ConfigPath: path}
	value := true
	if !r3KillAtStep(t, "manifest", func() error { _, err := ApplyManagedKey(deps, configSetKey, &value); return err }) {
		t.Fatal("the manifest step was not reached")
	}
	good := activationRead(t, path)
	activationWrite(t, path, good+"broken = \"unterminated\n")
	if r, err := ApplyManagedKey(deps, configSetKey, nil); r.OK || err == nil && r.Reason == "" {
		t.Fatalf("the unset went on over an invalid config: %+v %v", r, err)
	}
	if _, err := os.Stat(intentPath(home)); err != nil {
		t.Fatalf("the invalid config cost the intent: %v", err)
	}
	activationWrite(t, path, good)
	if r, err := ApplyManagedKey(deps, configSetKey, nil); err != nil || !r.OK || activationRead(t, path) != txOriginal {
		t.Fatalf("the unset after the file was fixed: %+v %v %q", r, err, activationRead(t, path))
	}
}
