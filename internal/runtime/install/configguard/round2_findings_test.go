package configguard

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/crwdir"
)

// The second verification round of this lane (CRW-1141, CRW-1143, CRW-1144, CRW-1145, CRW-1153) reproduced six defects with
// counterexamples; each test below is one of them, written to fail on the code that was verified and to pass on the fix.

// CRW-1143: a disable whose read-back has no row for the flag (unsupported) has not been shown to have disabled it; the
// ownership is kept and the manifest is not released.
func TestUnsupportedReadbackDoesNotProveDisabled(t *testing.T) {
	home, _, deps, state := txActivationFixture(t)
	if _, err := Activate(deps); err != nil {
		t.Fatal(err)
	}
	base := deps.Run
	lists := 0
	run := func(args []string) CodexRunResult {
		if args[1] == "disable" && args[2] == "goals" {
			return CodexRunResult{}
		}
		res := base(args)
		if args[1] == "list" {
			lists++
			if lists == 2 {
				rows := []string{}
				for _, row := range strings.Split(res.Stdout, "\n") {
					if !strings.HasPrefix(row, "goals ") {
						rows = append(rows, row)
					}
				}
				res.Stdout = strings.Join(rows, "\n")
			}
		}
		return res
	}
	r, err := Deactivate(deactivationDeps(home, run))
	if err != nil || !state["goals"] {
		t.Fatalf("fixture: %v %v", err, state)
	}
	failed := false
	for _, f := range r.Failed {
		failed = failed || f.Key == "goals"
	}
	for _, key := range r.Disabled {
		if key == "goals" {
			t.Fatalf("an unsupported read-back confirmed the disable: %+v", r)
		}
	}
	m := parseInstallManifest(activationRead(t, manifestPath(home)))
	if !failed || m.ReleasedAt != nil || !m.Flags["goals"].EnabledByCodexclaw {
		t.Fatalf("goals is not reported as unconfirmed, or its ownership was released: %+v released=%v", r, m.ReleasedAt)
	}
}

// CRW-1144: the deactivation re-proves the config lock after every disable runner. A runner that replaced the linked
// config.toml with a file another writer then locked stops the deactivation before the next disable.
func TestDeactivateRechecksLockBeforeNextRunner(t *testing.T) {
	home := activationHome(t)
	path := filepath.Join(home, "config.toml")
	target := filepath.Join(home, "target.toml")
	activationWrite(t, target, configLockPathsRunnerPost)
	if err := os.Symlink("target.toml", path); err != nil {
		t.Fatal(err)
	}
	m := &InstallManifest{Version: 2, ConfigPath: path, Flags: map[string]FlagRecord{}, TableKeys: map[string]TableKeyRecord{}}
	for _, k := range DeclaredFeatures() {
		m.Flags[string(k)] = FlagRecord{EnabledByCodexclaw: true}
	}
	b, err := manifestBytes(m)
	if err != nil {
		t.Fatal(err)
	}
	activationWrite(t, manifestPath(home), string(b))
	state := allActivationFlags()
	var other *crwdir.ConfigLock
	t.Cleanup(func() {
		if other != nil {
			other.Release()
		}
	})
	writes := []string{}
	run := func(args []string) CodexRunResult {
		if args[1] == "list" {
			return CodexRunResult{Stdout: configLockPathsFeatureListWith(state)}
		}
		if other != nil {
			writes = append(writes, args[2])
		} else {
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
			activationWrite(t, path, configLockPathsRunnerPost)
			var err error
			if other, err = crwdir.LockConfig(path, 0); err != nil {
				t.Fatal(err)
			}
		}
		state[args[2]] = false
		return CodexRunResult{}
	}
	r, err := Deactivate(deactivationDeps(home, run))
	if len(writes) > 0 || err == nil {
		t.Fatalf("disables ran while another writer holds the file config.toml names now: %v; result=%+v err=%v", writes, r, err)
	}
	if after := parseInstallManifest(activationRead(t, manifestPath(home))); after.ReleasedAt != nil {
		t.Fatal("the manifest was released by a deactivation that stopped")
	}
}

// CRW-1141: a [features.multi_agent_v2] table without an enabled key is restored with its tuning after the runner.
func TestV2TuningWithoutEnabledIsRestored(t *testing.T) {
	home, path := multiAgentHome(t)
	activationWrite(t, path, "[features.multi_agent_v2]\nmax = 7\n")
	var calls [][]string
	got, err := SetMultiAgentV2State(MultiAgentV2Deps{CodexHome: home, Run: multiAgentFake(t, path, &calls)}, MultiAgentV2)
	if err != nil || got == nil {
		t.Fatalf("the switch failed: %+v %v config=%q", got, err, activationRead(t, path))
	}
	want := "[features]\n\n[features.multi_agent_v2]\nenabled = true\nmax = 7\n"
	if config := activationRead(t, path); config != want {
		t.Fatalf("config = %q, want %q", config, want)
	}
}

// CRW-1145: a completed deactivation of an install that owned nothing releases the manifest too, so the next activation
// starts a new baseline from the flags as they are then.
func TestCompletedUnownedDisableStartsNewBaseline(t *testing.T) {
	home := activationHome(t)
	path := filepath.Join(home, "config.toml")
	activationWrite(t, path, "[memories]\ndedicated_tools = [true]\n")
	state := allActivationFlags()
	var calls [][]string
	deps := activationDeps(t, home, state, &calls)
	base := deps.Run
	deps.Run = func(args []string) CodexRunResult {
		if args[1] == "disable" {
			state[args[2]] = false
			activationWrite(t, path, SetTableKey(activationRead(t, path), "features", args[2], false).Content)
			return CodexRunResult{}
		}
		return base(args)
	}
	if _, err := Activate(deps); err != nil {
		t.Fatal(err)
	}
	if _, err := Deactivate(deactivationDeps(home, deps.Run)); err != nil {
		t.Fatal(err)
	}
	if m := parseInstallManifest(activationRead(t, manifestPath(home))); m.ReleasedAt == nil {
		t.Fatal("the completed deactivation did not release the install")
	}
	state["goals"] = false
	if _, err := Activate(deps); err != nil {
		t.Fatal(err)
	}
	rec := parseInstallManifest(activationRead(t, manifestPath(home))).Flags["goals"]
	if rec.PriorEnabled || !rec.EnabledByCodexclaw {
		t.Fatalf("the next activation kept the old baseline: %+v", rec)
	}
	if _, err := Deactivate(deactivationDeps(home, deps.Run)); err != nil || state["goals"] {
		t.Fatalf("the flag crw enabled after the new baseline was left on: %v %v", err, state)
	}
}
