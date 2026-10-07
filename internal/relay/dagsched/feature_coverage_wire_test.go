package dagsched

import (
	"bytes"
	"context"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport/golden"
)

// The wire shape of dag-feature-coverage (CRW-839 c3, pre-merge d5): the command's own JSON envelope,
// recorded as a golden so a field rename, a nullability change or a lost key is a visible diff rather than
// an unnoticed drift. It is the reader a parent scripts against, so the shape is part of the promise.
func TestFeatureCoverageWireShape(t *testing.T) {
	f := newFixture(t)
	putPacketPlan(t, f, "plan", 0, "r1",
		[]doc{featureCriteriaDoc("CRW-F", criterionDoc("c1", true), criterionDoc("c2", true), criterionDoc("c3", false))},
		packetNodeDoc("n1", "CRW-F", "p1", []string{"c1", "c3"}, []string{"c1", "c3"}),
		packetNodeDoc("n2", "CRW-F", "p2", []string{"c2", "c3"}, nil))
	packetExecution(f, "plan", "n1", "rel-1")
	packetExecution(f, "plan", "n2", "rel-2")
	liveRelationship(f, "plan", "n1", "rel-1")
	liveRelationship(f, "plan", "n2", "rel-2")
	packetAcceptance(f, "acc-1", "plan", "n1", "rel-1")
	packetAcceptance(f, "acc-2", "plan", "n2", "rel-2")
	packetRegistered(f, "rel-1", map[string]bool{"c1": true, "c3": true})
	packetRegistered(f, "rel-2", map[string]bool{"c2": true, "c3": true})
	packetIntegrated(f, "acc-1", "rel-1", "owner/repo", "dev")
	packetLanded(f, "train-1", "rel-1", acceptanceHead("rel-1"), "landed-1")

	cov, err := f.sched.FeatureCoverage(context.Background(), "plan", "CRW-F")
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := contract.Emit(&out, coverageAnswer(cov)); err != nil {
		t.Fatal(err)
	}
	// One of the two packets is integrated, so the reading is incomplete: the golden holds both the
	// packet list with its acceptance and integration and the criterion list with who covers and owns.
	golden.Check(t, "incomplete", out.Bytes(), golden.Substitute(acceptanceHead("rel-1"), "<HEAD-1>"))
}
