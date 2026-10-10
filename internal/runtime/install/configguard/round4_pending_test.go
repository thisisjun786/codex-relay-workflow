package configguard

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The fourth verification round of this lane (verify-r3 of 79537768d): an effect a recovery cannot attribute is kept as
// pending evidence until it is resolved, a flag is proven crw's by its own state in config.toml (not by any change of the
// file), and a retargeted config.toml is not the file a flag effect is about. Each test fails on 79537768d and passes on
// the fix.

// r4Pending answers the pending entries the intent holds, nil when there is no intent.
func r4Pending(t *testing.T, home string) []intentEffect {
	t.Helper()
	raw, err := os.ReadFile(intentPath(home))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	var in installIntent
	if err := json.Unmarshal(raw, &in); err != nil {
		t.Fatal(err)
	}
	for _, e := range in.Effects {
		if e.Attempted {
			t.Fatalf("the intent still holds an attempted effect after a command: %s", raw)
		}
	}
	return in.Pending
}

func r4PendingNames(t *testing.T, home string) []string {
	t.Helper()
	var names []string
	for _, e := range r4Pending(t, home) {
		names = append(names, e.Name)
	}
	return names
}

// CRW-1153: a flag runner that rewrote config.toml without enabling the flag, then a stop before the manifest: the flag the
// user enables afterwards is not crw's, because the done record says crw's run left it off. Nothing is pending either.
func TestDoneFlagTheRunnerLeftOffIsNotAdopted(t *testing.T) {
	home, path, deps, state := txActivationFixture(t)
	base := deps.Run
	deps.Run = func(args []string) CodexRunResult {
		if args[1] == "enable" && args[2] == "multi_agent" {
			activationWrite(t, path, activationRead(t, path)+"# unrelated CLI rewrite\n")
			return CodexRunResult{}
		}
		if args[1] == "enable" && args[2] == "goals" {
			return CodexRunResult{ExitCode: 2, Stderr: "hard failure"}
		}
		return base(args)
	}
	if !r3KillAtStep(t, "manifest", func() error { _, err := Activate(deps); return err }) {
		t.Fatal("the manifest step was not reached")
	}
	if state["multi_agent"] {
		t.Fatal("fixture: multi_agent was enabled")
	}
	state["multi_agent"] = true
	activationWrite(t, path, SetTableKey(activationRead(t, path), "features", "multi_agent", true).Content)
	r, err := Deactivate(deactivationDeps(home, deps.Run))
	if err != nil {
		t.Fatal(err)
	}
	if !state["multi_agent"] {
		t.Fatalf("the recovery took a change of other bytes for crw's flag change: %+v", r)
	}
	if names := r4PendingNames(t, home); len(names) != 0 {
		t.Fatalf("a flag the done record shows crw left off is kept pending: %v", names)
	}
}

// CRW-1153: an activation stopped before its manifest, then config.toml retargeted to another file the user's flags are on in:
// the recovery records nothing on the new file, the deactivation leaves its flags, and the effects on the original file (the
// four flags and the managed key) are kept pending.
func TestRetargetedFlagRecoveryKeepsPendingAndLeavesTheNewFile(t *testing.T) {
	home, path, deps, state := txActivationFixture(t)
	a, b := filepath.Join(home, "a.toml"), filepath.Join(home, "b.toml")
	if err := os.Rename(path, a); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("a.toml", path); err != nil {
		t.Fatal(err)
	}
	user := txOriginal + "[features]\nmulti_agent = true\ngoals = true\nhooks = true\ndefault_mode_request_user_input = true\n"
	activationWrite(t, b, user)
	if !r3KillAtStep(t, "manifest", func() error { _, err := Activate(deps); return err }) {
		t.Fatal("the manifest step was not reached")
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("b.toml", path); err != nil {
		t.Fatal(err)
	}
	r, err := Deactivate(deactivationDeps(home, deps.Run))
	if err != nil {
		t.Fatal(err)
	}
	if activationRead(t, b) != user || !state["multi_agent"] {
		t.Fatalf("the recovery carried the flags of a.toml to b.toml: %+v %q", r, activationRead(t, b))
	}
	if names := r4PendingNames(t, home); len(names) != 5 {
		t.Fatalf("the effects on a.toml are not kept pending: %v", names)
	}
	if len(r.Recovered) == 0 || !strings.Contains(strings.Join(r.Recovered, "\n"), "pending") {
		t.Fatalf("the deactivation does not report the pending effects: %+v", r)
	}
}

// CRW-1153: a set stopped after config.toml changed and before its done record: the recovery cannot tell crw's write from the
// user's, so the key is kept pending (not owned, not dropped); the unset says so and what to do; the explicit release
// resolves it.
func TestUnprovenKeyEffectIsKeptPendingUntilResolved(t *testing.T) {
	home, path := configSetHome(t, txOriginal, true)
	deps := ConfigSetDeps{CodexHome: home, ConfigPath: path}
	value := true
	n := 0
	txHook = func(s string) error {
		if s == "intent" {
			if n++; n == 2 {
				panic(txKilled{s})
			}
		}
		return nil
	}
	func() {
		defer func() {
			txHook = nil
			if recover() == nil {
				t.Fatal("the set was not stopped")
			}
		}()
		_, _ = ApplyManagedKey(deps, configSetKey, &value)
	}()
	written := activationRead(t, path)
	if !strings.Contains(written, "dedicated_tools = true") {
		t.Fatal("fixture: the set did not reach config.toml")
	}
	r, err := ApplyManagedKey(deps, configSetKey, nil)
	if err != nil || r.OK || activationRead(t, path) != written {
		t.Fatalf("the unset of an unproven key: %+v %v", r, err)
	}
	if configSetManifest(t, home).TableKeys[configSetKey].SetByCodexclaw {
		t.Fatal("an unproven key was recorded as crw's")
	}
	if names := r4PendingNames(t, home); len(names) != 1 || names[0] != configSetKey {
		t.Fatalf("the unproven key is not kept pending: %v", names)
	}
	if !strings.Contains(r.Reason, "pending") || !strings.Contains(r.Reason, "--release") {
		t.Fatalf("the unset does not report the uncertainty and the next step: %q", r.Reason)
	}
	// Another command keeps it.
	if r, err := ApplyManagedKey(deps, configSetKey, nil); err != nil || r.OK || len(r4PendingNames(t, home)) != 1 {
		t.Fatalf("the pending key was dropped by a later command: %+v %v", r, err)
	}
	rel, err := ReleaseManagedKey(deps, configSetKey)
	if err != nil || !rel.OK || activationRead(t, path) != written {
		t.Fatalf("the release of a pending key: %+v %v", rel, err)
	}
	if _, err := os.Stat(intentPath(home)); !os.IsNotExist(err) {
		t.Fatalf("the release left the pending intent: %v", err)
	}
}

// CRW-1153: a pending key that no longer holds the value crw was writing is resolved by the next command.
func TestPendingKeyThatNoLongerHoldsTheValueIsDropped(t *testing.T) {
	home, path := configSetHome(t, txOriginal, true)
	deps := ConfigSetDeps{CodexHome: home, ConfigPath: path}
	value := true
	n := 0
	txHook = func(s string) error {
		if s == "intent" {
			if n++; n == 2 {
				panic(txKilled{s})
			}
		}
		return nil
	}
	func() {
		defer func() { txHook = nil; _ = recover() }()
		_, _ = ApplyManagedKey(deps, configSetKey, &value)
	}()
	if _, err := ApplyManagedKey(deps, configSetKey, nil); err != nil || len(r4PendingNames(t, home)) != 1 {
		t.Fatalf("fixture: %v %v", err, r4PendingNames(t, home))
	}
	activationWrite(t, path, txOriginal)
	if r, err := ApplyManagedKey(deps, configSetKey, nil); err != nil || r.OK {
		t.Fatalf("%+v %v", r, err)
	}
	if _, err := os.Stat(intentPath(home)); !os.IsNotExist(err) {
		t.Fatalf("a pending key the user removed is still pending: %v", r4PendingNames(t, home))
	}
}

// CRW-1153: a flag that ran but whose done record a kill lost is kept pending across commands (config set, which cannot read
// flags, included), is left on by the deactivation, and is dropped once it is off.
func TestUnprovenFlagIsKeptPendingAcrossCommands(t *testing.T) {
	home, _, deps, state := txActivationFixture(t)
	n := 0
	txHook = func(s string) error {
		if s == "intent" {
			if n++; n == 3 { // the first intent, the multi_agent attempt, the goals attempt (which carries multi_agent's done)
				panic(txKilled{s})
			}
		}
		return nil
	}
	func() {
		defer func() { txHook = nil; _ = recover() }()
		_, _ = Activate(deps)
	}()
	if !state["multi_agent"] {
		t.Fatal("fixture: multi_agent did not run")
	}
	// An activation run again: the flag is on, so it is recorded as it is, and it stays pending.
	if _, err := Activate(deps); err != nil {
		t.Fatal(err)
	}
	if names := r4PendingNames(t, home); len(names) != 1 || names[0] != "multi_agent" {
		t.Fatalf("the unproven flag is not kept pending: %v", names)
	}
	value := false
	if r, err := ApplyManagedKey(ConfigSetDeps{CodexHome: home}, configSetKey, &value); err != nil || !r.OK {
		t.Fatalf("config set with a pending flag: %+v %v", r, err)
	}
	if names := r4PendingNames(t, home); len(names) != 1 {
		t.Fatalf("config set dropped the pending flag: %v", names)
	}
	r, err := Deactivate(deactivationDeps(home, deps.Run))
	if err != nil || !state["multi_agent"] {
		t.Fatalf("the deactivation turned off an unproven flag: %+v %v", r, err)
	}
	// A command that refuses (config set into a released install) still reports what it keeps pending.
	if r, err := ApplyManagedKey(ConfigSetDeps{CodexHome: home}, configSetKey, &value); err != nil || r.OK || !strings.Contains(strings.Join(r.Recovered, "\n"), "multi_agent") {
		t.Fatalf("a refusing command did not report the pending flag: %+v %v", r, err)
	}
	state["multi_agent"] = false
	if _, err := Activate(deps); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(intentPath(home)); !os.IsNotExist(err) {
		t.Fatalf("a pending flag that is off is still pending: %v", r4PendingNames(t, home))
	}
}
