package dagsched

import (
	"context"
	"testing"
)

// c4 (CRW-965): an implementation node that landed on a local integration branch with no pull request is observed
// there and counted as integrated by the progress reading. The completion is the observation and the parent's merged
// mark, not a merged pull request.
func TestLocalIntegrationCountsAsIntegratedWithoutAPullRequest(t *testing.T) {
	k := newLegacyLocalIntegrationKit(t)
	repo := k.repo
	repo.git("checkout", "-q", "-b", "feature")
	feature := repo.commit("feature.txt", "feature")
	repo.git("checkout", "-q", "dev")
	k.declare("g", "I", "feature.txt")
	a := k.acceptNode("g", "I", acceptOpts{HeadSHA: feature})
	k.holdSlotsFor("g", "I")
	repo.git("merge", "-q", "--no-ff", "-m", "merge feature", "feature")
	k.mark(a)
	res, err := k.observe()
	if err != nil || !res.Integrated {
		t.Fatalf("observe on the local branch = %v %+v; want integrated", err, res)
	}
	p, err := k.sched.ReadProgress(context.Background(), "g")
	if err != nil {
		t.Fatal(err)
	}
	if p.Cumulative.Integrated.Nodes != 1 {
		t.Fatalf("integrated count %d; want 1", p.Cumulative.Integrated.Nodes)
	}
	var node *NodeProgress
	for i := range p.Nodes {
		if p.Nodes[i].NodeID == "I" {
			node = &p.Nodes[i]
		}
	}
	if node == nil || node.Stage != StageIntegrated {
		t.Fatalf("node I = %+v; want stage integrated", node)
	}
	if node.Links.PullRequest != nil {
		t.Fatalf("node I names a pull request %+v; the path has none", node.Links.PullRequest)
	}
}
