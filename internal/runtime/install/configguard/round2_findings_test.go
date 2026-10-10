package configguard

import (
	"strings"
	"testing"
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
