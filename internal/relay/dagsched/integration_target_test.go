package dagsched

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport"
)

// CRW-1301 (decision A + D(i)): dag-integrate-push compares the remote head it reads with the heads the relay recorded for
// the integration ref (the base of each recorded intent and each head a ref_moved stage moved the ref onto). A head outside
// that set is a broken integration_target_moved fault in the fault ledger, whose blocking notice the daemon carries to the
// supervisor, and the push names it to the calling parent. The push does exactly what it did before. Each test runs in a
// temporary HOME, over a temporary store, with a temporary bare repository as origin.

// targetKit is a batch kit whose origin is a temporary bare repository on the base, with node a integrated onto dev-int.
func targetKit(t *testing.T) (*batchKit, string, IntegrationBatchResult) {
	t.Helper()
	testsupport.SandboxAccountHomes(t)
	k := newBatchKit(t, batchNode{name: "a", files: map[string]string{"a.txt": "a\n"}})
	remote := bareRemote(t)
	k.pushRepo(t, remote)
	k.repo.git("push", "-q", remote, k.base+":refs/heads/dev")
	k.acceptByCommit("a")
	res, err := k.sched.IntegrateBatch(context.Background(), k.batchIn(), IntegrationBatchDeps{Verify: stubVerifier(writeStubVerifier(t)), Update: updateIntegrationRef})
	if err != nil || len(res.Merged) != 1 {
		t.Fatalf("batch: %+v, %v", res, err)
	}
	return k, remote, res
}

// checkedPush is the push dag-integrate-push runs: the verified-move check, the stale list and the target check.
func checkedPush(k *batchKit) (PushResult, error) {
	return PushIntegrationChecked(context.Background(), k.repo.path, "origin", "dev", "dev-int", k.sched.VerifiedMoveOnto, k.sched.StaleIntegratedNodes, k.sched.CheckIntegrationTarget)
}

// targetFaults reads the integration_target_moved faults of the store.
func targetFaults(t *testing.T, k *batchKit) []store.Row {
	t.Helper()
	rows, err := k.sched.Store.Q(context.Background()).QueryContext(context.Background(), "SELECT fault_id FROM fault_ledger WHERE fault_class = ? ORDER BY fault_id", ClassIntegrationTargetMoved)
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	if err := rows.Close(); err != nil {
		t.Fatal(err)
	}
	var out []store.Row
	for _, id := range ids {
		r, err := k.sched.Store.One(context.Background(), "SELECT f.*, (SELECT COUNT(*) FROM fault_notifications n WHERE n.fault_id = f.fault_id AND n.kind = 'blocking' AND n.state = 'pending') AS blocking FROM fault_ledger f WHERE f.fault_id = ?", id)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, r)
	}
	return out
}

// TestIntegrationTargetMovedOutsidePathRaisesFault: a direct push to the temporary bare origin moves the target outside the
// integration path. The next push records the fault and names it, reverts nothing and pushes exactly as it did before: a
// target the integration branch contains is fast-forwarded, one it does not contain (r2 F0-1: a child pushed its own commit
// to dev) is refused with merge_base_mismatch and keeps its commit.
func TestIntegrationTargetMovedOutsidePathRaisesFault(t *testing.T) {
	k, remote, res := targetKit(t)
	var project string
	if row, err := k.sched.Store.One(context.Background(), "SELECT project_key FROM dag_plans WHERE plan_id = 'g'"); err != nil || row == nil {
		t.Fatalf("plan g: %v", err)
	} else {
		project = row.Text("project_key")
	}

	// a commit the integration branch contains, but not a head the relay recorded: the node's own head, pushed directly
	contained := k.heads["a"]
	if contained == res.NewHead || contained == k.base {
		t.Fatalf("the node head %s must differ from the recorded heads %s and %s", contained, k.base, res.NewHead)
	}
	k.repo.git("push", "-q", remote, contained+":refs/heads/dev")
	pushed, err := checkedPush(k)
	if err != nil || pushed.Outcome != PushPushed {
		t.Fatalf("push over a moved target the branch contains: %+v, %v; want pushed, as before", pushed, err)
	}
	if got := remoteBranch(t, remote, "dev"); got != res.NewHead {
		t.Fatalf("remote dev is %s; want the fast-forward to %s", got, res.NewHead)
	}
	m := pushed.TargetMoved
	if m == nil || pushed.TargetCheckError != "" {
		t.Fatalf("the push does not name the moved target: %+v", pushed)
	}
	if m.IntegrationRef != "dev-int" || m.RemoteHead != contained || m.ExpectedTotal != 2 || !slices.Equal(m.Expected, []string{res.NewHead, k.base}) || m.FaultID == "" || m.FaultError != "" {
		t.Fatalf("target_moved: %+v; want dev-int, remote %s, expected [%s %s], a recorded fault", m, contained, res.NewHead, k.base)
	}
	faults := targetFaults(t, k)
	if len(faults) != 1 {
		t.Fatalf("%d integration_target_moved faults; want 1", len(faults))
	}
	f := faults[0]
	if f.Text("fault_id") != m.FaultID || f.Text("product") != "crw" || f.Text("severity") != "broken" || f.Text("state") != "open" || f.Get("occurrence_count") != int64(1) || f.Get("blocking") != int64(1) {
		t.Fatalf("fault: %v; want product crw, broken, open, one occurrence and one pending blocking notice", f)
	}
	if !strings.Contains(f.Text("scope"), `"projectKey": "`+project+`"`) || !strings.Contains(f.Text("signature"), `"remoteRef":"dev"`) {
		t.Fatalf("fault scope %s, signature %s; want the plan's project %s and the remote ref", f.Text("scope"), f.Text("signature"), project)
	}

	// r2 F0-1: a child pushes its own commit, which the integration branch does not contain, straight to dev
	k.repo.git("checkout", "-q", "-B", "child", res.NewHead)
	k.repo.write("child.txt", "child\n")
	k.repo.git("add", "child.txt")
	k.repo.git("commit", "-q", "-m", "child")
	child := k.repo.git("rev-parse", "HEAD")
	k.repo.git("checkout", "-q", "dev")
	k.repo.git("push", "-q", remote, child+":refs/heads/dev")
	refused, err := checkedPush(k)
	var detail *store.RefusedError
	if refusalReasonOf(err) != "merge_base_mismatch" || !errors.As(err, &detail) {
		t.Fatalf("push over a child's commit: %v; want merge_base_mismatch, as before", err)
	}
	if !strings.Contains(detail.Detail, ClassIntegrationTargetMoved) || !strings.Contains(detail.Detail, child) || !strings.Contains(detail.Detail, m.FaultID) {
		t.Fatalf("the refusal does not name the moved target: %s", detail.Detail)
	}
	if refused.TargetMoved == nil || refused.TargetMoved.RemoteHead != child || refused.TargetMoved.FaultID != m.FaultID {
		t.Fatalf("refused push result: %+v", refused.TargetMoved)
	}
	if got := remoteBranch(t, remote, "dev"); got != child {
		t.Fatalf("remote dev is %s; the child's commit %s must stay: nothing is reverted", got, child)
	}
	// the push without the check answers the same: the check changes nothing the push does
	if _, err := PushIntegration(context.Background(), k.repo.path, "origin", "dev", "dev-int", k.sched.VerifiedMoveOnto, nil); refusalReasonOf(err) != "merge_base_mismatch" || strings.Contains(err.Error(), ClassIntegrationTargetMoved) {
		t.Fatalf("unchecked push: %v; want the same merge_base_mismatch, naming no fault", err)
	}
	faults = targetFaults(t, k)
	if len(faults) != 1 || faults[0].Get("occurrence_count") != int64(2) || faults[0].Get("blocking") != int64(1) || faults[0].Text("state") != "open" {
		t.Fatalf("faults after the second move: %v; want the same fault with two occurrences and one blocking notice", faults)
	}
}

// A remote on a head the relay recorded is no fault: the first push finds the base an intent recorded and the second the
// head the relay moved the ref onto. Nothing is recorded and the result is the one the unchecked push gives.
func TestIntegrationTargetOnPathRecordsNoFault(t *testing.T) {
	k, remote, res := targetKit(t)
	first, err := checkedPush(k)
	if err != nil || first.Outcome != PushPushed || first.TargetMoved != nil || first.TargetCheckError != "" {
		t.Fatalf("first push from the recorded base: %+v, %v; want pushed and no moved target", first, err)
	}
	if got := remoteBranch(t, remote, "dev"); got != res.NewHead {
		t.Fatalf("remote dev is %s; want %s", got, res.NewHead)
	}
	second, err := checkedPush(k)
	if err != nil || second.Outcome != PushUpToDate || second.TargetMoved != nil || second.TargetCheckError != "" {
		t.Fatalf("second push on the moved head: %+v, %v; want up_to_date and no moved target", second, err)
	}
	plain, err := PushIntegration(context.Background(), k.repo.path, "origin", "dev", "dev-int", k.sched.VerifiedMoveOnto, k.sched.StaleIntegratedNodes)
	if err != nil || !reflect.DeepEqual(plain, second) {
		t.Fatalf("checked %+v and unchecked %+v (%v) differ", second, plain, err)
	}
	if faults := targetFaults(t, k); len(faults) != 0 {
		t.Fatalf("a target on the path recorded %d faults", len(faults))
	}
}

// A remote that cannot be read defers the push as before and records no fault, even though the target has moved: nothing was
// read to compare, and the comparison never reads the remote again.
func TestIntegrationTargetUnreadableRemoteRecordsNoFault(t *testing.T) {
	k, remote, _ := targetKit(t)
	k.repo.git("push", "-q", remote, k.heads["a"]+":refs/heads/dev")
	k.repo.git("remote", "set-url", "origin", "http://127.0.0.1:1/codex-relay-workflow.git")
	deferred, err := checkedPush(k)
	if err != nil || deferred.Outcome != PushDeferred || deferred.RemoteHead != "" || deferred.TargetMoved != nil || deferred.TargetCheckError != "" {
		t.Fatalf("push to an unreachable remote: %+v, %v; want deferred, no moved target and no check error", deferred, err)
	}
	if got := remoteBranch(t, remote, "dev"); got != k.heads["a"] {
		t.Fatalf("remote dev is %s; want it untouched at %s", got, k.heads["a"])
	}
	if faults := targetFaults(t, k); len(faults) != 0 {
		t.Fatalf("an unreadable remote recorded %d faults", len(faults))
	}
}
