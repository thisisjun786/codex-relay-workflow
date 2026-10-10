package configguard

import (
	"path/filepath"
	"strings"
	"testing"
)

// The third verification round of this lane, the deactivation: an owned flag the first listing has no row for, a flags-only
// install on a config.toml that does not decode, and a disable that was not read back. Each test fails on the code that was
// verified (c3081dc3) and passes on the fix.

func r3Strip(list, flag string) string {
	rows := []string{}
	for _, row := range strings.Split(list, "\n") {
		if !strings.HasPrefix(row, flag+" ") {
			rows = append(rows, row)
		}
	}
	return strings.Join(rows, "\n")
}

// CRW-1141, CRW-1143, CRW-1144, CRW-1145, CRW-1149: an owned flag the FIRST listing has no row for is not disabled-already.
// The disable still runs for it while config.toml holds it on, the install stays live while the flag is not confirmed off, and
// the retry (the row back) finishes it.
func TestOwnedFlagMissingFromTheFirstListingKeepsTheInstallLive(t *testing.T) {
	home, _, deps, state := txActivationFixture(t)
	if _, err := Activate(deps); err != nil {
		t.Fatal(err)
	}
	if !state["goals"] {
		t.Fatal("fixture: goals is not on")
	}
	base := deps.Run
	omit := true
	run := func(args []string) CodexRunResult {
		res := base(args)
		if args[1] == "list" && omit {
			res.Stdout = r3Strip(res.Stdout, "goals")
		}
		return res
	}
	r, err := Deactivate(deactivationDeps(home, run))
	if err != nil {
		t.Fatal(err)
	}
	m := parseInstallManifest(activationRead(t, manifestPath(home)))
	failed := false
	for _, f := range r.Failed {
		failed = failed || f.Key == "goals"
	}
	if m.ReleasedAt != nil || !failed || !m.Flags["goals"].EnabledByCodexclaw {
		t.Fatalf("an owned flag the listing did not show was released as already disabled: %+v released=%v", r, m.ReleasedAt)
	}
	if state["goals"] {
		t.Fatal("the disable never reached goals, which config.toml holds on")
	}
	omit = false
	if r, err = Deactivate(deactivationDeps(home, run)); err != nil || len(r.Failed) != 0 {
		t.Fatalf("the retry with the row back: %+v %v", r, err)
	}
	if m = parseInstallManifest(activationRead(t, manifestPath(home))); m.ReleasedAt == nil {
		t.Fatal("the retry did not release the install")
	}
}

// A flag with no row that config.toml does not hold on has nothing to disable: the deactivation is complete.
func TestOwnedFlagWithoutRowAndNotOnInConfigIsNothingToDisable(t *testing.T) {
	home, path, deps, state := txActivationFixture(t)
	if _, err := Activate(deps); err != nil {
		t.Fatal(err)
	}
	state["goals"] = false
	activationWrite(t, path, SetTableKey(activationRead(t, path), "features", "goals", false).Content)
	base := deps.Run
	run := func(args []string) CodexRunResult {
		res := base(args)
		if args[1] == "list" {
			res.Stdout = r3Strip(res.Stdout, "goals")
		}
		return res
	}
	r, err := Deactivate(deactivationDeps(home, run))
	if err != nil || len(r.Failed) != 0 {
		t.Fatalf("%+v %v", r, err)
	}
	if m := parseInstallManifest(activationRead(t, manifestPath(home))); m.ReleasedAt == nil {
		t.Fatal("a flag with nothing to disable kept the install live")
	}
}

// CRW-1141: a flags-only install on a config.toml that does not decode is refused before any CLI disable.
func TestDeactivateRefusesAnInvalidConfigForAFlagsOnlyInstall(t *testing.T) {
	home := activationHome(t)
	path := filepath.Join(home, "config.toml")
	activationWrite(t, path, "[features]\ngoals = true\nbroken = \"unterminated\n")
	m := &InstallManifest{Version: 2, ConfigPath: path, Flags: map[string]FlagRecord{"goals": {EnabledByCodexclaw: true}}, TableKeys: map[string]TableKeyRecord{}}
	m.flagOrder = []string{"goals"}
	b, err := manifestBytes(m)
	if err != nil {
		t.Fatal(err)
	}
	activationWrite(t, manifestPath(home), string(b))
	state := map[string]bool{"goals": true}
	var calls [][]string
	run := activationRun(t, home, state, &calls)
	disables := 0
	r, err := Deactivate(deactivationDeps(home, func(args []string) CodexRunResult {
		if args[1] == "disable" {
			disables++
		}
		return run(args)
	}))
	if err == nil || disables != 0 {
		t.Fatalf("a flags-only install reached the CLI on an invalid config: %+v %v disables=%d", r, err, disables)
	}
	if after := parseInstallManifest(activationRead(t, manifestPath(home))); after.ReleasedAt != nil {
		t.Fatal("the install was released")
	}
}
