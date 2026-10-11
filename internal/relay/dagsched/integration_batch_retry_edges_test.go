package dagsched

import (
	"strings"
	"testing"
)

// retryCandidates over the plan edges: a removed candidate is retried only when every prerequisite of it in the batch is a
// survivor or is retried with it.
func TestRetryCandidatesNeedsEveryRemovedPrerequisite(t *testing.T) {
	cand := func(n, reason string) removedCandidate {
		return removedCandidate{Candidate: Candidate{NodeID: n}, reason: reason}
	}
	nodes := func(ns ...string) []Candidate {
		var out []Candidate
		for _, n := range ns {
			out = append(out, Candidate{NodeID: n})
		}
		return out
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
	if got := ids(retryCandidates(nodes("a", "b", "c", "e"), nodes("a"), removed, edges)); got != "b,c,e" {
		t.Fatalf("chain: retried %q; want b,c,e", got)
	}
	// two prerequisites, both retried: the dependant is retried
	edges = map[string][]string{"b": {"c"}, "x": {"c"}}
	removed = []removedCandidate{cand("b", "not_proven_failing"), cand("x", "not_proven_failing"), cand("c", "depends_on_b")}
	if got := ids(retryCandidates(nodes("b", "c", "x"), nil, removed, edges)); got != "b,c,x" {
		t.Fatalf("both recovered: retried %q; want b,c,x", got)
	}
	// one prerequisite failed alone: the dependant and its own dependants wait
	edges = map[string][]string{"b": {"c"}, "d": {"c"}, "c": {"e"}}
	removed = []removedCandidate{cand("b", "not_proven_failing"), cand("d", "verification_failed"), cand("c", "depends_on_b"), cand("e", "depends_on_b")}
	if got := ids(retryCandidates(nodes("b", "c", "d", "e"), nil, removed, edges)); got != "b" {
		t.Fatalf("one prerequisite excluded: retried %q; want b only", got)
	}
	// a prerequisite that a conflict deferred and a cascade excluded is not among the removed, and still keeps its dependant out
	edges = map[string][]string{"b": {"c", "d"}, "d": {"c"}}
	removed = []removedCandidate{cand("b", "not_proven_failing"), cand("c", "depends_on_b")}
	if got := ids(retryCandidates(nodes("a", "b", "c", "d"), nodes("a"), removed, edges)); got != "b" {
		t.Fatalf("deferred prerequisite: retried %q; want b only", got)
	}
	// a not_proven_failing candidate whose own prerequisite failed is not retried
	edges = map[string][]string{"d": {"b"}}
	removed = []removedCandidate{cand("b", "not_proven_failing"), cand("d", "verification_failed")}
	if got := ids(retryCandidates(nodes("a", "b", "c", "d"), nodes("a", "c"), removed, edges)); got != "" {
		t.Fatalf("failed prerequisite: retried %q; want none", got)
	}
	// a prerequisite reached through a node outside the batch counts: d -> y -> b with y not a candidate
	edges = map[string][]string{"d": {"y"}, "y": {"b"}}
	if got := ids(retryCandidates(nodes("a", "b", "c", "d"), nodes("a", "c"), removed, edges)); got != "" {
		t.Fatalf("prerequisite through an outside node: retried %q; want none", got)
	}
	// a survivor prerequisite does not hold a candidate back
	edges = map[string][]string{"a": {"b"}}
	if got := ids(retryCandidates(nodes("a", "b", "c", "d"), nodes("a", "c"), removed, edges)); got != "b" {
		t.Fatalf("survivor prerequisite: retried %q; want b", got)
	}
	// a conflict-deferred candidate that a cascade excluded is among the removed and is retried with its prerequisite
	edges = map[string][]string{"b": {"e"}}
	removed = []removedCandidate{cand("b", "not_proven_failing"), {Candidate: Candidate{NodeID: "e"}, reason: "depends_on_b", deferred: true}}
	if got := ids(retryCandidates(nodes("a", "b", "d", "e"), nodes("a"), removed, edges)); got != "b,e" {
		t.Fatalf("deferred dependant: retried %q; want b,e", got)
	}
}
