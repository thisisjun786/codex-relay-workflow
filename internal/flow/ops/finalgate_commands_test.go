package ops

import "testing"

func TestReviewUnknownCommandFinalGate(t *testing.T) {
	w := newCloseWorld(t, Milestone{ID: "M1", Plan: "plan-1", Title: "m", CriteriaDigest: "crit", Criteria: []string{"c"}, RequiredCommands: []string{"make e2e"}})
	w.lane.d.Verifier = RepositoryVerifier{Fallback: w.verifier}
	got := w.run(`
const g = await crw.finalGate('M1')
for (const command of g.need_tests) await crw.verify('', g.head, {milestone:'M1', command, base:'base-tip'})
const again = await crw.finalGate('M1')
return {state:again.state, need:again.need_tests}
`)
	if got["state"] != GatePass {
		t.Fatalf("configured verification passed, but final gate: %v", got)
	}
	for _, row := range rows(w.j.Events()).opRows(OpVerify) {
		if dataStr(row, "command") != CILocalCommand || dataStr(row, "verifier_note") == "" {
			t.Fatalf("actual command or warning lost: %+v", row)
		}
	}
}
