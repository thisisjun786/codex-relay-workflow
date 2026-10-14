package dagsched

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport"
)

// CRW-1304: the integration target record. The relay records which remote branch an integration ref is pushed to (an explicit command,
// the --remote and --remote-ref flags of dag-integrate, and the push once it has read the remote head), compares the push and the
// checker with the heads of that integration ref alone once the target is recorded, and leaves the CRW-1301 comparison as it was for a
// ref with no record. Each test runs in a temporary HOME over a temporary store, with a temporary bare repository as origin.

func targetRows(t *testing.T, k *batchKit) []store.IntegrationTargetRow {
	t.Helper()
	ctx := context.Background()
	rows, err := k.sched.Store.Q(ctx).QueryContext(ctx, "SELECT kind, checkout, integration_ref, remote, remote_ref, plan_id, observed_head, source, recorded_by FROM dag_integration_targets ORDER BY rowid")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []store.IntegrationTargetRow
	for rows.Next() {
		var r store.IntegrationTargetRow
		if err := rows.Scan(&r.Kind, &r.Checkout, &r.IntegrationRef, &r.Remote, &r.RemoteRef, &r.PlanID, &r.ObservedHead, &r.Source, &r.RecordedBy); err != nil {
			t.Fatal(err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

func (k *batchKit) recordTarget(t *testing.T, ref, remoteRef string) store.IntegrationTargetRow {
	t.Helper()
	row, _, err := k.sched.RecordTarget(context.Background(), TargetInput{Checkout: k.repo.path, IntegrationRef: ref, Remote: "origin", RemoteRef: remoteRef, Actor: "parent", Source: store.TargetSourceRecord})
	if err != nil {
		t.Fatalf("record target of %s: %v", ref, err)
	}
	return row
}

// The explicit record names the plan of the ref's newest head, appends a row once, and appends again only when the remote or the remote ref changes. The
// ref must be one a batch recorded, the checkout an absolute path, and no name may start with a dash: those are input errors, and nothing is written.
func TestRecordTargetAppendsOnceAndRefusesBadInput(t *testing.T) {
	k, _, _ := targetKit(t)
	ctx := context.Background()
	in := TargetInput{Checkout: k.repo.path, IntegrationRef: "dev-int", Remote: "origin", RemoteRef: "dev", Actor: "parent", Source: store.TargetSourceRecord}
	row, appended, err := k.sched.RecordTarget(ctx, in)
	if err != nil || !appended || row.PlanID != "g" || row.Kind != store.TargetKindTarget || row.RecordedBy != "parent" {
		t.Fatalf("first record: %+v, %v, %v; want an appended target row of plan g", row, appended, err)
	}
	if _, appended, err := k.sched.RecordTarget(ctx, in); err != nil || appended {
		t.Fatalf("the same record again: %v, %v; want a no-op", appended, err)
	}
	moved := in
	moved.RemoteRef, moved.Actor = "main", "someone"
	if row, appended, err := k.sched.RecordTarget(ctx, moved); err != nil || !appended || row.RemoteRef != "main" {
		t.Fatalf("a changed remote ref: %+v, %v, %v; want appended", row, appended, err)
	}
	if rows := targetRows(t, k); len(rows) != 2 {
		t.Fatalf("%d rows after one repeat and one change; want 2: %+v", len(rows), rows)
	}
	before := len(targetRows(t, k))
	for name, mutate := range map[string]func(*TargetInput){
		"an integration ref no batch recorded": func(i *TargetInput) { i.IntegrationRef = "nope-int" },
		"a remote starting with a dash":        func(i *TargetInput) { i.Remote = "-origin" },
		"a remote ref starting with a dash":    func(i *TargetInput) { i.RemoteRef = "--dev" },
		"a checkout starting with a dash":      func(i *TargetInput) { i.Checkout = "-C" },
		"a relative checkout":                  func(i *TargetInput) { i.Checkout = "repo" },
		"an empty actor":                       func(i *TargetInput) { i.Actor = "" },
	} {
		bad := in
		mutate(&bad)
		if _, appended, err := k.sched.RecordTarget(ctx, bad); err == nil || appended || refusalReasonOf(err) != "merge_target_unreadable" {
			t.Errorf("%s: %v, %v; want a merge_target_unreadable refusal", name, appended, err)
		}
	}
	if after := len(targetRows(t, k)); after != before {
		t.Fatalf("refused records left %d rows; want %d", after, before)
	}
}

// dag-integrate-push records the target once it has read the remote head, and the head the remote holds afterwards. The push is what it was: the
// comparison of that push ran without the record, and a second push finds the record and the observation.
func TestPushRecordsTargetAndObservation(t *testing.T) {
	k, remote, res := targetKit(t)
	ctx := context.Background()
	pushed, err := checkedPush(k)
	if err != nil || pushed.Outcome != PushPushed || pushed.TargetMoved != nil {
		t.Fatalf("push: %+v, %v", pushed, err)
	}
	if err := k.sched.RecordPushRead(ctx, k.repo.path, "origin", "dev", "dev-int", pushed.RemoteHead); err != nil {
		t.Fatal(err)
	}
	rows := targetRows(t, k)
	if len(rows) != 2 || rows[0].Kind != store.TargetKindTarget || rows[0].Source != store.TargetSourcePush || rows[0].RecordedBy != PushActor || rows[0].PlanID != "g" ||
		rows[1].Kind != store.TargetKindObserved || rows[1].ObservedHead != res.NewHead || rows[1].RemoteRef != "dev" {
		t.Fatalf("rows after the push: %+v; want the target of dev-int on origin/dev and the head %s the push left there", rows, res.NewHead)
	}
	if got := remoteBranch(t, remote, "dev"); got != res.NewHead {
		t.Fatalf("remote dev is %s; want %s", got, res.NewHead)
	}
	// the same push again changes nothing in the record
	again, err := checkedPush(k)
	if err != nil || again.Outcome != PushUpToDate {
		t.Fatalf("second push: %+v, %v", again, err)
	}
	if err := k.sched.RecordPushRead(ctx, k.repo.path, "origin", "dev", "dev-int", again.RemoteHead); err != nil {
		t.Fatal(err)
	}
	if after := targetRows(t, k); len(after) != 2 {
		t.Fatalf("a push that found the same head wrote rows: %+v", after)
	}
	// a ref no batch recorded is not recorded by a push: the error is the push's to report, not a refusal of the push
	if err := k.sched.RecordPushRead(ctx, k.repo.path, "origin", "dev", "nope-int", res.NewHead); err == nil {
		t.Fatal("a target of an unknown integration ref was recorded")
	}
}

// c3: with the target recorded, the push compares against the ref's own heads only. Without a record the heads of the other integration refs are
// on the path (the existing TestIntegrationTargetAnotherRefOnPathRecordsNoFault, unchanged); with it, another ref's head on the target's remote
// branch is a move outside the path of this ref, and the refusal and the outcome are the ones the push gave before.
func TestRecordedTargetComparesWithOwnHeadsOnly(t *testing.T) {
	testsupport.SandboxAccountHomes(t)
	k := newBatchKit(t, batchNode{name: "a", files: map[string]string{"a.txt": "a\n"}}, batchNode{name: "b", files: map[string]string{"b.txt": "b\n"}})
	remote := bareRemote(t)
	k.pushRepo(t, remote)
	k.repo.git("push", "-q", remote, k.base+":refs/heads/dev")
	headA := k.integrateNodes(t, "dev-int", "a")
	k.integrateNodes(t, "other-int", "b")
	if first, err := checkedPush(k); err != nil || first.Outcome != PushPushed || first.TargetMoved != nil {
		t.Fatalf("push of dev-int: %+v, %v; want pushed and no moved target", first, err)
	}
	other := func() (PushResult, error) {
		return PushIntegrationChecked(context.Background(), k.repo.path, "origin", "dev", "other-int", k.sched.VerifiedMoveOnto, k.sched.StaleIntegratedNodes, k.sched.CheckIntegrationTarget)
	}
	// no record for other-int: dev-int's head is on the path, as before
	if refused, err := other(); refusalReasonOf(err) != "merge_base_mismatch" || refused.TargetMoved != nil {
		t.Fatalf("push of other-int without a record: %+v, %v; want merge_base_mismatch and no moved target", refused, err)
	}
	k.recordTarget(t, "other-int", "dev")
	refused, err := other()
	var detail *store.RefusedError
	if refusalReasonOf(err) != "merge_base_mismatch" || !errors.As(err, &detail) {
		t.Fatalf("push of other-int with its target recorded: %v; want merge_base_mismatch, as before", err)
	}
	m := refused.TargetMoved
	if m == nil || !m.Strict || m.Source != SourcePush || m.RemoteHead != headA || m.IntegrationRef != "other-int" || m.FaultID == "" {
		t.Fatalf("moved target of the recorded other-int: %+v; want dev-int's head %s named as outside other-int's path, recorded by the push", m, headA)
	}
	if !strings.Contains(detail.Detail, ClassIntegrationTargetMoved) || strings.Contains(detail.Detail, "any other integration ref") {
		t.Fatalf("refusal detail %q; want the moved target named without the words that count other refs' heads", detail.Detail)
	}
	if got := remoteBranch(t, remote, "dev"); got != headA {
		t.Fatalf("remote dev is %s; want dev-int's head %s untouched", got, headA)
	}
	if faults := targetFaults(t, k); len(faults) != 1 {
		t.Fatalf("%d faults; want the one fault of the recorded target", len(faults))
	}
	// dev-int's own push over its own recorded head is still on its path
	k.recordTarget(t, "dev-int", "dev")
	if again, err := checkedPush(k); err != nil || again.Outcome != PushUpToDate || again.TargetMoved != nil {
		t.Fatalf("push of recorded dev-int on its own head: %+v, %v; want up_to_date and no moved target", again, err)
	}
}
