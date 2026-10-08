package dagsched

import (
	"context"
	"testing"
)

// CRW-952 c5: a record judged under criteria that were re-registered is premerge_criteria_stale. The record is built
// before the change, so it names the old criteria digest; the change is a plain re-registration with no revalidation.

// TestPremergeCLIReRegisteredCriteriaMakeTheRecordStaleAtAccept: dag-accept through the binary refuses the old record.
func TestPremergeCLIReRegisteredCriteriaMakeTheRecordStaleAtAccept(t *testing.T) {
	k := newCommitAcceptKit(t)
	raw := premergeRaw(t, premergeRecordOf(t, k))
	k.invRevise("g", "I", "g-r2", func(n doc) { n["criteria_set_digest"] = dig("re-registered I") })
	k.report("g", "I")
	m, code := premergeCLIRun(t, k, raw)
	premergeCLIExpect(t, m, code, "premerge_criteria_stale")
}

// TestPremergeIntegrationReRegisteredCriteriaLeaveTheCandidateOut: a candidate accepted under its record, whose criteria are
// re-registered afterwards, is not a ready candidate any more. The existing stale rule removes it before the record is
// read, so dag-integrate names it with the stale refusal the DAG feature already uses (disposition_conflict), not with
// premerge_criteria_stale. This is the reading the handoff records for the parent to accept (CRW-952 c5 wording).
func TestPremergeIntegrationReRegisteredCriteriaLeaveTheCandidateOut(t *testing.T) {
	k := newBatchKit(t, batchNode{name: "a", files: map[string]string{"a.txt": "a\n"}})
	k.acceptByCommit("a")
	k.invRevise("g", "a", "g-r2", func(n doc) { n["criteria_set_digest"] = dig("re-registered a") })
	in := k.batchIn()
	in.Nodes = []string{"a"}
	_, err := k.sched.IntegrateBatch(context.Background(), in, IntegrationBatchDeps{Verify: stubVerifier(writeStubVerifier(t)), Update: updateIntegrationRef})
	if got := refusalReasonOf(err); got != "disposition_conflict" {
		t.Fatalf("a named candidate whose criteria were re-registered: reason %q (err %v); want disposition_conflict", got, err)
	}
}
