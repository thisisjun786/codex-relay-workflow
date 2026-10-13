package role

import (
	"strings"
	"testing"
)

// CRW-1330 (evaluation 5a1a64c2 d2, criterion c1): a flow-only key the store holds in mode default is undecided, like a helper
// role in mode default; only a key the store does not hold (or cannot use) takes the compiled default.
func TestCRW1330_FlowOnlyKeyInModeDefaultIsUndecided(t *testing.T) {
	env, dir := home(t)
	writeStore(t, dir, `{"roles": {"executor-t1": {"mode": "default"}, "reviewer-final": {"mode": "default", "effort": "high"}}}`)
	snap := ReadFlowSnapshot(env)
	got := flowPairs(snap)
	for _, r := range []RoleName{ExecutorT1, ReviewerFinal} {
		if got[r] != [3]string{"", "", "undecided"} {
			t.Errorf("%s: %v", r, got[r])
		}
	}
	joined := strings.Join(snap.Warnings, "\n")
	for _, want := range []string{"role executor-t1 is in mode default", "role reviewer-final is in mode default", "undecided"} {
		if !strings.Contains(joined, want) {
			t.Errorf("warnings lack %q:\n%s", want, joined)
		}
	}
	if strings.Contains(joined, "the flow uses the compiled default") {
		t.Errorf("a key in mode default is not at its compiled default:\n%s", joined)
	}
	// get answers the role as the flow reads it
	view := helperCLIDecode[map[string]any](t, RunHelper(ParseHelperArgs([]string{"get", "executor-t1"}), env))
	if view["mode"] != "default" || view["model"] != nil || view["source"] != "undecided" {
		t.Fatalf("get of a flow key in mode default: %v", view)
	}

	// A set of the effort alone on a missing key stores mode default: the answer and the snapshot say it is undecided, it is
	// not silently the compiled default.
	env2, _ := home(t)
	set := helperCLIDecode[map[string]any](t, RunHelper(ParseHelperArgs([]string{"set", "executor-t1", "--effort", "high"}), env2))
	if set["mode"] != "default" || set["source"] != "undecided" || set["effort"] != "high" {
		t.Fatalf("set of the effort alone: %v", set)
	}
	if p := flowPairs(ReadFlowSnapshot(env2))[ExecutorT1]; p != [3]string{"", "", "undecided"} {
		t.Fatalf("the snapshot after set --effort: %v", p)
	}
	// mode default over a model makes it undecided; reset removes the key and brings back the compiled default
	RunHelper(ParseHelperArgs([]string{"set", "executor-t1", "--mode", "model", "--model", "m-t1"}), env2)
	if p := flowPairs(ReadFlowSnapshot(env2))[ExecutorT1]; p != [3]string{"m-t1", "high", "store"} {
		t.Fatalf("after set --mode model: %v", p)
	}
	set = helperCLIDecode[map[string]any](t, RunHelper(ParseHelperArgs([]string{"set", "executor-t1", "--mode", "default"}), env2))
	if set["source"] != "undecided" || flowPairs(ReadFlowSnapshot(env2))[ExecutorT1][2] != "undecided" {
		t.Fatalf("set --mode default: %v", set)
	}
	reset := helperCLIDecode[map[string]any](t, RunHelper(ParseHelperArgs([]string{"reset", "executor-t1"}), env2))
	if reset["source"] != "compiled_default" || flowPairs(ReadFlowSnapshot(env2))[ExecutorT1] != [3]string{"anthropic/claude-sonnet-5-5", "xhigh", "compiled_default"} {
		t.Fatalf("reset: %v", reset)
	}
}
