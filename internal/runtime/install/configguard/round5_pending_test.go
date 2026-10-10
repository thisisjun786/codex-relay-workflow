package configguard

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The fifth verification round of this lane (verify-r4 of 637b6b03f): a pending entry is resolved only by evidence that the
// effect is no longer in place. A config.toml crw cannot read is no such evidence, and neither is the state of another file
// config.toml was retargeted to: the effect is about the file it was written to. Each test fails on 637b6b03f and passes on
// the fix.

// r5KillSetBeforeDone stops a config set after config.toml changed and before its done record.
func r5KillSetBeforeDone(t *testing.T, deps ConfigSetDeps) {
	t.Helper()
	n := 0
	txHook = func(s string) error {
		if s == "intent" {
			if n++; n == 2 {
				panic(txKilled{s})
			}
		}
		return nil
	}
	defer func() {
		txHook = nil
		if recover() == nil {
			t.Fatal("the set was not stopped before its done record")
		}
	}()
	value := true
	_, _ = ApplyManagedKey(deps, configSetKey, &value)
}

// CRW-1153: a pending key stays pending, and is reported, while config.toml does not decode or holds the key in a form crw
// does not read: whether the key still holds crw's value is unknown, not resolved.
func TestPendingKeyIsKeptWhileConfigTomlCannotBeRead(t *testing.T) {
	for name, edit := range map[string]func(string) string{
		"invalid-toml": func(s string) string { return s + "[invalid\n" },
		"unsupported-form": func(string) string {
			return "# mine\nmemories = { generate_memories = true, dedicated_tools = true }\n"
		},
	} {
		t.Run(name, func(t *testing.T) {
			home, path := configSetHome(t, txOriginal, true)
			deps := ConfigSetDeps{CodexHome: home, ConfigPath: path}
			r5KillSetBeforeDone(t, deps)
			if _, err := ApplyManagedKey(deps, configSetKey, nil); err != nil {
				t.Fatal(err)
			}
			if names := r4PendingNames(t, home); len(names) != 1 {
				t.Fatalf("fixture: the unproven key is not pending: %v", names)
			}
			activationWrite(t, path, edit(activationRead(t, path)))
			r, err := ApplyManagedKey(deps, configSetKey, nil)
			if err != nil {
				t.Fatal(err)
			}
			if names := r4PendingNames(t, home); len(names) != 1 || names[0] != configSetKey {
				t.Fatalf("a config.toml crw cannot read resolved the pending key: %v", names)
			}
			if !strings.Contains(strings.Join(r.Recovered, "\n"), "pending") {
				t.Fatalf("the command does not report the key it keeps pending: %+v", r)
			}
		})
	}
}

// r5Retarget points config.toml (a symlink) at name, a file holding content.
func r5Retarget(t *testing.T, home, path, name, content string) {
	t.Helper()
	activationWrite(t, filepath.Join(home, name), content)
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(name, path); err != nil {
		t.Fatal(err)
	}
}

// r5LinkToA moves config.toml to a.toml and points config.toml at it.
func r5LinkToA(t *testing.T, home, path string) string {
	t.Helper()
	a := filepath.Join(home, "a.toml")
	if err := os.Rename(path, a); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("a.toml", path); err != nil {
		t.Fatal(err)
	}
	return a
}

// CRW-1153: a set stopped before its done record, then config.toml retargeted before any recovery to a file without the
// key: the key crw may have written stays in a.toml, so it is kept pending (and a.toml is left as it is).
func TestRetargetedKeyEffectIsKeptPendingWhenTheNewFileLacksIt(t *testing.T) {
	home, path := configSetHome(t, txOriginal, true)
	a := r5LinkToA(t, home, path)
	deps := ConfigSetDeps{CodexHome: home, ConfigPath: path}
	r5KillSetBeforeDone(t, deps)
	effect := activationRead(t, a)
	r5Retarget(t, home, path, "b.toml", txOriginal)
	r, err := ApplyManagedKey(deps, configSetKey, nil)
	if err != nil {
		t.Fatal(err)
	}
	if activationRead(t, a) != effect || activationRead(t, path) != txOriginal {
		t.Fatal("the recovery changed a config file")
	}
	pending := r4Pending(t, home)
	if len(pending) != 1 || pending[0].Name != configSetKey || pending[0].Target != a {
		t.Fatalf("the key effect left in a.toml is not kept pending for a.toml: %+v", pending)
	}
	if !strings.Contains(strings.Join(r.Recovered, "\n"), "pending") {
		t.Fatalf("the command does not report the key it keeps pending: %+v", r)
	}
	// It stays while config.toml names another file.
	if _, err := ApplyManagedKey(deps, configSetKey, nil); err != nil || len(r4PendingNames(t, home)) != 1 {
		t.Fatalf("a later command dropped the pending key: %v %v", err, r4PendingNames(t, home))
	}
}

// CRW-1153: an activation stopped before its manifest, then config.toml retargeted before any recovery to a file the flags
// are off in: the flags crw turned on in a.toml, and the key it set there, are kept pending for a.toml, not forgotten.
func TestRetargetedFlagEffectsAreKeptPendingWhenTheNewFileHasThemOff(t *testing.T) {
	home, path, deps, state := txActivationFixture(t)
	a := r5LinkToA(t, home, path)
	if !r3KillAtStep(t, "manifest", func() error { _, err := Activate(deps); return err }) {
		t.Fatal("the manifest step was not reached")
	}
	r5Retarget(t, home, path, "b.toml", txOriginal)
	for name := range state {
		state[name] = false
	}
	r, err := Deactivate(deactivationDeps(home, deps.Run))
	if err != nil {
		t.Fatal(err)
	}
	pending := r4Pending(t, home)
	flags := 0
	for _, e := range pending {
		if e.Target != a || e.Kind != intentFlag && e.Name != "memories.dedicated_tools" {
			t.Fatalf("a pending entry is not an effect on a.toml: %+v", e)
		}
		if e.Kind == intentFlag {
			flags++
		}
	}
	if flags != 4 || len(pending) != 5 {
		t.Fatalf("the effects left in a.toml are not kept pending: %+v", pending)
	}
	if !strings.Contains(strings.Join(r.Recovered, "\n"), "pending") {
		t.Fatalf("the deactivation does not report the pending effects: %+v", r)
	}
}
