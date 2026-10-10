package configguard

import (
	"testing"
)

// The second verification round of this lane (CRW-1141, CRW-1143, CRW-1144, CRW-1145, CRW-1153) reproduced six defects with
// counterexamples; each test below is one of them, written to fail on the code that was verified and to pass on the fix.

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
