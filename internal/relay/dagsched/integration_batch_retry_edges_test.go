package dagsched

import (
	"strings"
	"testing"
)

// retryCandidates over the plan edges: a dependant is retried only when every prerequisite that was removed is in the retry set.
func TestRetryCandidatesNeedsEveryRemovedPrerequisite(t *testing.T) {
	cand := func(n, reason string) removedCandidate {
		return removedCandidate{Candidate: Candidate{NodeID: n}, reason: reason}
	}
	ids := func(cs []Candidate) string {
		var out []string
		for _, c := range cs {
			out = append(out, c.NodeID)
		}
		return strings.Join(out, ",")
	}
	// chain b -> c -> e with b not proven: all three are retried
	edges := map[string][]string{"b": {"c"}, "c": {"e"}}
	removed := []removedCandidate{cand("b", "not_proven_failing"), cand("c", "depends_on_b"), cand("e", "depends_on_b")}
	if got := ids(retryCandidates(removed, edges)); got != "b,c,e" {
		t.Fatalf("chain: retried %q; want b,c,e", got)
	}
	// two prerequisites, both retried: the dependant is retried
	edges = map[string][]string{"b": {"c"}, "x": {"c"}}
	removed = []removedCandidate{cand("b", "not_proven_failing"), cand("x", "not_proven_failing"), cand("c", "depends_on_b")}
	if got := ids(retryCandidates(removed, edges)); got != "b,c,x" {
		t.Fatalf("both recovered: retried %q; want b,c,x", got)
	}
	// one prerequisite failed alone: the dependant and its own dependants wait
	edges = map[string][]string{"b": {"c"}, "d": {"c"}, "c": {"e"}}
	removed = []removedCandidate{cand("b", "not_proven_failing"), cand("d", "verification_failed"), cand("c", "depends_on_b"), cand("e", "depends_on_b")}
	if got := ids(retryCandidates(removed, edges)); got != "b" {
		t.Fatalf("one prerequisite excluded: retried %q; want b only", got)
	}
}
