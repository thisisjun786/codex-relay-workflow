package trace

import "testing"

// TestStageOfReadsEveryRoleAnOldRunRecorded: a call's stage on its node is read from the role its row names, and a trace export reads the
// rows old runs wrote as they are. The role key reviewer-final is retired (CRW-1450); a call an old run made under it starts the stage
// final_review, as it did, and the roles of the current lane start theirs.
func TestStageOfReadsEveryRoleAnOldRunRecorded(t *testing.T) {
	for role, want := range map[string]string{
		"executor": "build", "executor-t1": "build", "reviewer": "review", "reviewer-final": "final_review", "architect": "plan", "explorer": "explore",
		"critic": "critic",
	} {
		if got := stageOf(role); got != want {
			t.Errorf("stageOf(%q) = %q, want %q", role, got, want)
		}
	}
}
