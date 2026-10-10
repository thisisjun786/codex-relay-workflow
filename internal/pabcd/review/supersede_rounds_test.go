package review

import (
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/goalplan"
)

// CRW-1100: ObsoleteRounds lists every open round of the purpose this session owns under another epoch, and
// SupersedeRounds closes exactly the listed ones that still qualify, keeps a cursor that names a round it did
// not close, and closes nothing twice.
func TestSupersedeRoundsClosesTheListedObsoleteRoundsOnly(t *testing.T) {
	round := func(id string, purpose goalplan.ReviewPurpose, owner, epoch string, status goalplan.ReviewRoundStatus) goalplan.ReviewRoundState {
		return goalplan.ReviewRoundState{RoundID: id, Purpose: purpose, Status: status, OwnerSessionID: owner, PlanEpoch: epoch}
	}
	p := &goalplan.Goalplan{ReviewRounds: []goalplan.ReviewRoundState{
		round("r1", goalplan.PurposePlanAudit, "s", "e1", goalplan.ReviewInFlight),
		round("r2", goalplan.PurposePlanAudit, "s", "e2", goalplan.ReviewPending),
		round("r3", goalplan.PurposePlanAudit, "other", "e1", goalplan.ReviewInFlight),
		round("r4", goalplan.PurposeFinalGate, "s", "e1", goalplan.ReviewInFlight),
		round("r5", goalplan.PurposePlanAudit, "s", "e2", goalplan.ReviewApproved),
		round("r6", goalplan.PurposePlanAudit, "s", "new", goalplan.ReviewInFlight),
		round("r7", goalplan.PurposePlanAudit, "s", "", goalplan.ReviewInFlight),
	}}
	cursor := "r3"
	p.ActivePlanAuditRoundID = &cursor
	ids := ObsoleteRounds(p, goalplan.PurposePlanAudit, "s", "new")
	if strings.Join(ids, ",") != "r1,r2" {
		t.Fatalf("obsolete rounds = %v, want [r1 r2]", ids)
	}
	out, closed := SupersedeRounds(p, goalplan.PurposePlanAudit, "s", "new", ids, "")
	if strings.Join(closed, ",") != "r1,r2" {
		t.Fatalf("closed = %v", closed)
	}
	if out.ActivePlanAuditRoundID == nil || *out.ActivePlanAuditRoundID != "r3" {
		t.Errorf("the cursor on a round this call did not close was cleared: %v", out.ActivePlanAuditRoundID)
	}
	if again, closedAgain := SupersedeRounds(out, goalplan.PurposePlanAudit, "s", "new", ids, ""); len(closedAgain) != 0 || again != out {
		t.Errorf("a replay closed %v again", closedAgain)
	}
	p.ActivePlanAuditRoundID = new("r2")
	if out, _ := SupersedeRounds(p, goalplan.PurposePlanAudit, "s", "new", ids, ""); out.ActivePlanAuditRoundID != nil {
		t.Errorf("the cursor on a closed round was kept: %v", *out.ActivePlanAuditRoundID)
	}
}
