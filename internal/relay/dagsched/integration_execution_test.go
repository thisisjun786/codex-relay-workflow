package dagsched

import (
	"context"
	"testing"
)

// CRW-429: the cleanup of a finished child asks whether the plan node it executed has landed, tied to the merged mark that counts now: another event, generation or revision all answer "not
// integrated", and a relationship that executed no node is not a plan node at all.
func TestExecutionIntegratedIsTiedToTheCurrentMark(t *testing.T) {
	k := newIntegrationKit(t)
	repo := k.repo
	repo.git("checkout", "-q", "-b", "feature")
	feature := repo.commit("feature.txt", "feature")
	repo.git("checkout", "-q", "dev")
	k.declare("g", "I", "feature.txt")
	a := k.acceptNode("g", "I", acceptOpts{HeadSHA: feature, PR: 5, Forge: "owner/repo", Repository: repo.path})
	k.holdSlotsFor("g", "I")
	rid, gen, event, revision := a.Acceptance.RelationshipID, a.Acceptance.ExecutionGeneration, a.Event, a.Acceptance.RevisionHash
	if n := k.count("SELECT COUNT(*) FROM dag_node_executions WHERE relationship_id = '" + rid + "'"); n != 1 {
		t.Fatalf("the scenario judges %d executions, want 1", n)
	}
	judge := func(want string, rel, ev string, g int64, rev string) {
		t.Helper()
		applicable, integrated, err := k.sched.ExecutionIntegrated(context.Background(), rel, ev, g, rev)
		if got := map[bool]string{true: "integrated", false: "not integrated"}[integrated]; err != nil || applicable != (want != "none") || (want != "none" && got != want) {
			t.Fatalf("%s: applicable=%v integrated=%v err=%v, want %s", ev, applicable, integrated, err, want)
		}
	}
	judge("not integrated", rid, event, gen, revision) // accepted, not landed
	repo.git("merge", "-q", "--no-ff", "-m", "merge feature", "feature")
	if _, err := k.observe(); err != nil {
		t.Fatal(err)
	}
	judge("not integrated", rid, event, gen, revision) // landed, not marked
	k.mark(a)
	if _, err := k.observe(); err != nil {
		t.Fatal(err)
	}
	judge("integrated", rid, event, gen, revision)
	judge("not integrated", rid, "ev-other", gen, revision)
	judge("not integrated", rid, event, gen+1, revision)
	judge("not integrated", rid, event, gen, "other-hash")
	judge("none", "relationship-without-a-node", event, gen, revision)
}
