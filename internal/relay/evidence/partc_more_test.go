package evidence

import "testing"

// The uncovered candidate-state branches required by the property inventory have no Python
// counterpart; this is the one explicitly required Go-only gap test.
func Test24_MEE_Gap_CandidateStateBranches(t *testing.T) {
	for _, state := range []string{"blocked", "has_hooks", "draft"} {
		candidate := map[string]any{"state": "open", "merged": false, "isDraft": false, "mergeStateStatus": state}
		p := CandidateProblems(candidate, false)
		if state == "has_hooks" && len(p) != 0 {
			t.Fatal(p)
		}
		if state != "has_hooks" && len(p) == 0 {
			t.Fatal(state)
		}
	}
}
