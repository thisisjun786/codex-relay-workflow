package faults

import (
	"context"
	"fmt"
	"testing"
)

// Every FN replay below runs the same CLI transitions in Go and Python (live, or its recorded
// answers). f1ReplayCLI compares exit status, stdout, stderr, and every fault_* table row
// after each transition, so the assertions cover the complete notification
// state rather than a hand-picked projection.
//
// The hybrid notice replays these tests also ran - the original Python NoticeCase scenarios with
// Go's NoticeDeliverer, StageNotice, ComposeNotice and ParkNotice bridged in, the fixture's store
// handed between the two runtimes at every step - were cross-runtime interop and are gone:
// rollback to Python closed at todo 43 (rollback_allowed=0) and the Python runtime leaves in
// todo 44 (docs/port/oracles/g3.md).
func fnReplayFault(t *testing.T) (context.Context, string, string, string) {
	t.Helper()
	ctx, gd, pd := f1ReplayStores(t)
	f1ReplayCLI(t, ctx, gd, pd, []string{"fault-target", "--product", "crw", "--project", "P", "--team", "team", "--project-ref", "project-P"})
	answer := f1ReplayCLI(t, ctx, gd, pd, []string{"fault-observe", "--observation", `{"schema":"fault-observation/1","product":"crw","faultClass":"report_omitted","severity":"broken","signature":{"relationship":"rel","turn":"turn"},"occurrenceKey":"one","scope":{"projectKey":"P"}}`})
	return ctx, gd, pd, answer["faultId"].(string)
}

func fnRaise(t *testing.T, ctx context.Context, gd, pd, fault, reason string) map[string]any {
	t.Helper()
	return f1ReplayCLI(t, ctx, gd, pd, []string{"fault-notification-raise", "--fault", fault, "--reason", reason})
}

func fnReserve(t *testing.T, ctx context.Context, gd, pd string) map[string]any {
	t.Helper()
	answer := f1ReplayCLI(t, ctx, gd, pd, []string{"fault-notification-reserve", "--owner", "worker", "--limit", "1"})
	return answer["reserved"].([]any)[0].(map[string]any)
}

func Test22_FN_3_DecisionWholeOutput(t *testing.T) {
	ctx, gd, pd, id := fnReplayFault(t)
	n := fnRaise(t, ctx, gd, pd, id, "operator-choice")
	r := fnReserve(t, ctx, gd, pd)
	f1ReplayCLI(t, ctx, gd, pd, []string{"fault-notification-ack", "--notification", n["notificationId"].(string), "--token", r["token"].(string), "--ref", "decision-message"})
}

func Test22_FN_4_InvalidIdentifiersWholeOutput(t *testing.T) {
	ctx, gd, pd, id := fnReplayFault(t)
	fnRaise(t, ctx, gd, pd, id, "classification")
	f1SeedBoth(t, ctx, gd, pd, []string{"UPDATE fault_ledger SET product='bad' || char(10) WHERE fault_id='" + id + "'"})
	f1ReplayCLI(t, ctx, gd, pd, []string{"fault-notifications"})
	f1ReplayCLI(t, ctx, gd, pd, []string{"fault-notification-reserve", "--owner", "worker", "--limit", "1"})
}

func Test22_FN_5_IneligibleLevelSpendsNothingWholeOutput(t *testing.T) {
	ctx, gd, pd, id := fnReplayFault(t)
	fnRaise(t, ctx, gd, pd, id, "classification")
	f1ReplayCLI(t, ctx, gd, pd, []string{"fault-notifications"})
	f1ReplayCLI(t, ctx, gd, pd, []string{"fault-notification-reserve", "--owner", "worker", "--limit", "1"})
	f1ReplayCLI(t, ctx, gd, pd, []string{"fault-attention"})
}

func Test22_FN_6_UncertainSettlementWholeOutput(t *testing.T) {
	ctx, gd, pd, id := fnReplayFault(t)
	fnRaise(t, ctx, gd, pd, id, "classification")
	n := fnReserve(t, ctx, gd, pd)
	f1SeedBoth(t, ctx, gd, pd, []string{"UPDATE fault_notifications SET lease_until=0 WHERE notification_id='" + n["notificationId"].(string) + "'"})
	f1ReplayCLI(t, ctx, gd, pd, []string{"fault-notifications", "--notification-state", "uncertain"})
	f1ReplayCLI(t, ctx, gd, pd, []string{"fault-notification-reconcile", "--notification", n["notificationId"].(string), "--delivered", "yes", "--ref", "host-read"})
	// FN-6 / m10: another caller cannot attest arrival for the daemon's
	// transport when the supervisor channel has no recorded send.
	f1SeedBoth(t, ctx, gd, pd, []string{"UPDATE fault_notifications SET state='uncertain',owner='relay-daemon',delivered_at=NULL,ack_ref=NULL WHERE notification_id='" + n["notificationId"].(string) + "'"})
	f1ReplayCLI(t, ctx, gd, pd, []string{"fault-notification-reconcile", "--notification", n["notificationId"].(string), "--delivered", "yes", "--ref", "somebody says it arrived"})
}

func Test22_FN_7_BudgetAndRefundWholeOutput(t *testing.T) {
	ctx, gd, pd, id := fnReplayFault(t)
	fnRaise(t, ctx, gd, pd, id, "one")
	fnRaise(t, ctx, gd, pd, id, "two")
	f1ReplayCLI(t, ctx, gd, pd, []string{"fault-limit", "--product", "crw", "--kind", "notification", "--max-count", "1", "--window", "3600"})
	r := fnReserve(t, ctx, gd, pd)
	f1ReplayCLI(t, ctx, gd, pd, []string{"fault-notification-fail", "--notification", r["notificationId"].(string), "--token", r["token"].(string), "--error", "not sent"})
	f1ReplayCLI(t, ctx, gd, pd, []string{"fault-notification-reserve", "--owner", "worker", "--limit", "2"})
}

func Test22_FN_10_WithdrawnNoticeWholeOutput(t *testing.T) {
	ctx, gd, pd, id := fnReplayFault(t)
	fnRaise(t, ctx, gd, pd, id, "classification")
	f1SeedBoth(t, ctx, gd, pd, []string{"UPDATE fault_ledger SET state='withdrawn' WHERE fault_id='" + id + "'", "UPDATE fault_notifications SET state='withdrawn' WHERE fault_id='" + id + "'"})
	f1ReplayCLI(t, ctx, gd, pd, []string{"fault-notifications", "--notification-state", "withdrawn"})
	fnRaise(t, ctx, gd, pd, id, "classification")
	f1ReplayCLI(t, ctx, gd, pd, []string{"fault-notifications"})
}

func Test22_FN_11_CurrentProjectAddressWholeOutput(t *testing.T) {
	ctx, gd, pd, id := fnReplayFault(t)
	fnRaise(t, ctx, gd, pd, id, "classification")
	f1ReplayCLI(t, ctx, gd, pd, []string{"fault-move", "--fault", id, "--scope", `{"projectKey":"OTHER"}`})
	f1ReplayCLI(t, ctx, gd, pd, []string{"fault-notifications"})
}

func Test22_FN_12_ProjectFaultWholeOutput(t *testing.T) {
	ctx, gd, pd := f1ReplayStores(t)
	f1ReplayCLI(t, ctx, gd, pd, []string{"fault-target", "--product", "crw", "--project", "P", "--team", "team", "--project-ref", "project-P"})
	a := f1ReplayCLI(t, ctx, gd, pd, []string{"fault-observe", "--observation", `{"schema":"fault-observation/1","product":"crw","faultClass":"managed_start_failed","severity":"broken","signature":{"issueKey":"PROJECT","receiptStatus":"failed"},"occurrenceKey":"managed:one:failed","scope":{"projectKey":"P"}}`})
	fnRaise(t, ctx, gd, pd, a["faultId"].(string), "classification")
	f1ReplayCLI(t, ctx, gd, pd, []string{"fault-notifications"})
}

func Test22_FN_13_AnchorWishesWholeOutput(t *testing.T) {
	ctx, gd, pd, id := fnReplayFault(t)
	f1SeedBoth(t, ctx, gd, pd, []string{f1Relationship, "INSERT INTO relationship_scope VALUES('rel','P','stamp')", "UPDATE relationships SET status='paused' WHERE relationship_id='rel'"})
	fnRaise(t, ctx, gd, pd, id, "classification")
	f1ReplayCLI(t, ctx, gd, pd, []string{"fault-notifications"})
	f1ReplayCLI(t, ctx, gd, pd, []string{"fault-notification-reserve", "--owner", "worker", "--limit", "1"})
}

func Test22_FN_14_OneBlockedRecipientDoesNotHideOthersWholeOutput(t *testing.T) {
	ctx, gd, pd, id := fnReplayFault(t)
	for i := 0; i < 3; i++ {
		fnRaise(t, ctx, gd, pd, id, fmt.Sprintf("decision-%d", i))
	}
	f1ReplayCLI(t, ctx, gd, pd, []string{"fault-limit", "--product", "crw", "--kind", "notification", "--max-count", "2", "--window", "3600"})
	f1ReplayCLI(t, ctx, gd, pd, []string{"fault-notification-reserve", "--owner", "worker", "--limit", "3"})
	f1ReplayCLI(t, ctx, gd, pd, []string{"fault-notifications"})
}
