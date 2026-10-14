package dagsched

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport"
)

// CRW-1304: the one-shot check of the recorded integration targets. It reads each recorded remote with the push's own git calls, compares the
// head with the heads recorded for the integration ref, records the broken integration_target_moved fault for a head outside them (one
// occurrence per remote head) and stores the head it read. It never reverts, pushes or blocks; a remote it cannot read is deferred.

func (k *batchKit) check(t *testing.T) []TargetReading {
	t.Helper()
	got, err := k.sched.CheckRecordedTargets(context.Background(), "", "")
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	return got
}

func oneReading(t *testing.T, got []TargetReading) TargetReading {
	t.Helper()
	if len(got) != 1 {
		t.Fatalf("%d readings; want 1: %+v", len(got), got)
	}
	return got[0]
}

func (k *batchKit) observations(t *testing.T) []string {
	t.Helper()
	var heads []string
	for _, r := range targetRows(t, k) {
		if r.Kind == store.TargetKindObserved {
			heads = append(heads, r.ObservedHead)
		}
	}
	return heads
}

// A remote on a head the relay recorded is on the path: no fault, and the head is stored. The base an intent recorded and the head a ref_moved
// stage moved the ref onto are both on it. A second run reads the same head and writes nothing.
func TestCheckTargetOnPathRecordsNoFaultAndStoresTheHead(t *testing.T) {
	k, remote, res := targetKit(t)
	k.recordTarget(t, "dev-int", "dev")
	first := oneReading(t, k.check(t))
	if first.Status != TargetOnPath || first.RemoteHead != k.base || first.Moved != nil || !first.Observed || first.ObservationError != "" {
		t.Fatalf("reading on the recorded base: %+v; want on_path on %s, observed", first, k.base)
	}
	k.repo.git("push", "-q", remote, res.NewHead+":refs/heads/dev")
	second := oneReading(t, k.check(t))
	if second.Status != TargetOnPath || second.RemoteHead != res.NewHead || !second.Observed {
		t.Fatalf("reading on the moved head: %+v; want on_path on %s", second, res.NewHead)
	}
	third := oneReading(t, k.check(t))
	if third.Status != TargetOnPath || third.Observed {
		t.Fatalf("a repeated run: %+v; want on_path and nothing stored", third)
	}
	if got := k.observations(t); len(got) != 2 || got[0] != k.base || got[1] != res.NewHead {
		t.Fatalf("observations %v; want [%s %s]", got, k.base, res.NewHead)
	}
	if faults := targetFaults(t, k); len(faults) != 0 {
		t.Fatalf("an on-path remote recorded %d faults", len(faults))
	}
}

// A head outside the recorded set records the fault once per remote head, with the source check in its evidence, no second blocking notice, and
// nothing reverted: the remote keeps the head. A re-run on the same head is idempotent; a further move adds an occurrence.
func TestCheckTargetMovedRecordsOneFaultPerHead(t *testing.T) {
	k, remote, res := targetKit(t)
	k.recordTarget(t, "dev-int", "dev")
	foreign := k.heads["a"]
	if foreign == res.NewHead || foreign == k.base {
		t.Fatalf("the node head %s must differ from the recorded heads", foreign)
	}
	k.repo.git("push", "-q", remote, foreign+":refs/heads/dev")
	first := oneReading(t, k.check(t))
	m := first.Moved
	if first.Status != TargetMovedOutside || m == nil || m.FaultID == "" || m.FaultError != "" || m.RemoteHead != foreign || !m.Strict || m.Source != SourceCheck || m.ExpectedTotal != 2 || !first.Observed {
		t.Fatalf("reading of a moved target: %+v / %+v; want status moved, a recorded fault of the check and an observation", first, m)
	}
	faults := targetFaults(t, k)
	if len(faults) != 1 {
		t.Fatalf("%d faults; want 1", len(faults))
	}
	f := faults[0]
	if f.Text("fault_id") != m.FaultID || f.Text("severity") != "broken" || f.Text("state") != "open" || f.Get("occurrence_count") != int64(1) || f.Get("blocking") != int64(1) {
		t.Fatalf("fault %v; want broken, open, one occurrence and one blocking notice", f)
	}
	if !strings.Contains(f.Text("signature"), `"remoteRef":"dev"`) || !strings.Contains(f.Text("signature"), `"remote":"origin"`) {
		t.Fatalf("fault signature %s; want the checkout, remote and remote ref", f.Text("signature"))
	}
	if evidence := k.faultEvidence(t, m.FaultID); !strings.Contains(evidence, `"source":"check"`) || !strings.Contains(evidence, foreign) {
		t.Fatalf("fault evidence %s; want the source check and the head %s", evidence, foreign)
	}
	if got := remoteBranch(t, remote, "dev"); got != foreign {
		t.Fatalf("remote dev is %s; the check must revert nothing and push nothing (want %s)", got, foreign)
	}
	// the same head again: the same fault, no new occurrence, no second blocking notice, no new observation
	again := oneReading(t, k.check(t))
	if again.Status != TargetMovedOutside || again.Moved.FaultID != m.FaultID || again.Observed {
		t.Fatalf("re-run: %+v; want the same fault and nothing stored", again)
	}
	faults = targetFaults(t, k)
	if len(faults) != 1 || faults[0].Get("occurrence_count") != int64(1) || faults[0].Get("blocking") != int64(1) {
		t.Fatalf("faults after a re-run: %v; want the same single occurrence and blocking notice", faults)
	}
	// a further move is another occurrence of the same fault, and still one blocking notice
	k.repo.git("checkout", "-q", "-B", "child", res.NewHead)
	k.repo.write("child.txt", "child\n")
	k.repo.git("add", "child.txt")
	k.repo.git("commit", "-q", "-m", "child")
	child := k.repo.git("rev-parse", "HEAD")
	k.repo.git("checkout", "-q", "dev")
	k.repo.git("push", "-q", "-f", remote, child+":refs/heads/dev")
	if next := oneReading(t, k.check(t)); next.Status != TargetMovedOutside || next.Moved.RemoteHead != child {
		t.Fatalf("reading after a further move: %+v", next)
	}
	faults = targetFaults(t, k)
	if len(faults) != 1 || faults[0].Get("occurrence_count") != int64(2) || faults[0].Get("blocking") != int64(1) {
		t.Fatalf("faults after a further move: %v; want two occurrences and one blocking notice", faults)
	}
	if got := k.observations(t); len(got) != 2 || got[0] != foreign || got[1] != child {
		t.Fatalf("observations %v; want [%s %s]", got, foreign, child)
	}
}

func (k *batchKit) faultEvidence(t *testing.T, id string) string {
	t.Helper()
	ctx := context.Background()
	rows, err := k.sched.Store.Q(ctx).QueryContext(ctx, "SELECT evidence FROM fault_occurrences WHERE fault_id = ? ORDER BY recorded_ts", id)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var all []string
	for rows.Next() {
		var e string
		if err := rows.Scan(&e); err != nil {
			t.Fatal(err)
		}
		all = append(all, strings.ReplaceAll(e, " ", ""))
	}
	return strings.Join(all, "\n")
}

// A remote that cannot be reached, a branch the remote does not hold and a checkout git cannot read are deferred: nothing is faulted, nothing is
// stored, and the check answers every target it was asked about. The unreachable remote is the one the existing push test uses.
func TestCheckTargetDefersWhatItCannotRead(t *testing.T) {
	k, remote, _ := targetKit(t)
	ctx := context.Background()
	k.repo.git("push", "-q", remote, k.heads["a"]+":refs/heads/dev") // the target has moved, but none of the readings below can see it
	k.recordTarget(t, "dev-int", "nobranch")
	absent := oneReading(t, k.check(t))
	if absent.Status != TargetAbsent || absent.RemoteHead != "" || absent.Moved != nil || absent.Observed || !strings.Contains(absent.Detail, "nobranch") {
		t.Fatalf("a branch the remote lacks: %+v; want absent, nothing compared", absent)
	}
	k.recordTarget(t, "dev-int", "dev")
	k.repo.git("remote", "set-url", "origin", "http://127.0.0.1:1/codex-relay-workflow.git")
	unreachable := oneReading(t, k.check(t))
	if unreachable.Status != TargetUnreachable || unreachable.Moved != nil || unreachable.Observed || unreachable.Detail == "" {
		t.Fatalf("an unreachable remote: %+v; want unreachable, nothing compared", unreachable)
	}
	gone := filepath.Join(t.TempDir(), "gone")
	if _, _, err := k.sched.RecordTarget(ctx, TargetInput{Checkout: gone, IntegrationRef: "dev-int", Remote: "origin", RemoteRef: "dev", Actor: "parent", Source: store.TargetSourceRecord}); err != nil {
		t.Fatal(err)
	}
	missing, err := k.sched.CheckRecordedTargets(ctx, gone, "")
	if err != nil {
		t.Fatal(err)
	}
	if r := oneReading(t, missing); r.Status != TargetUnreadable || r.Moved != nil || r.Observed {
		t.Fatalf("a checkout that is gone: %+v; want unreadable, nothing compared", r)
	}
	if faults := targetFaults(t, k); len(faults) != 0 {
		t.Fatalf("deferred readings recorded %d faults", len(faults))
	}
	if got := k.observations(t); len(got) != 0 {
		t.Fatalf("deferred readings stored observations %v", got)
	}
}

// c3 for the checker: two refs recorded on the same remote branch. The head dev-int's push left there is on dev-int's path and outside other-int's.
func TestCheckTargetsOfTwoRefsOnOneRemoteBranch(t *testing.T) {
	testsupport.SandboxAccountHomes(t)
	k := newBatchKit(t, batchNode{name: "a", files: map[string]string{"a.txt": "a\n"}}, batchNode{name: "b", files: map[string]string{"b.txt": "b\n"}})
	remote := bareRemote(t)
	k.pushRepo(t, remote)
	k.repo.git("push", "-q", remote, k.base+":refs/heads/dev")
	headA := k.integrateNodes(t, "dev-int", "a")
	k.integrateNodes(t, "other-int", "b")
	k.recordTarget(t, "dev-int", "dev")
	k.recordTarget(t, "other-int", "dev")
	k.repo.git("push", "-q", remote, headA+":refs/heads/dev")
	got := k.check(t)
	if len(got) != 2 || got[0].Target.IntegrationRef != "dev-int" || got[1].Target.IntegrationRef != "other-int" {
		t.Fatalf("readings %+v; want dev-int then other-int", got)
	}
	if got[0].Status != TargetOnPath || got[1].Status != TargetMovedOutside || got[1].Moved.RemoteHead != headA {
		t.Fatalf("statuses %s and %s; want on_path for dev-int and moved for other-int (dev-int's head is none of other-int's)", got[0].Status, got[1].Status)
	}
	// a filter reads one target
	only, err := k.sched.CheckRecordedTargets(context.Background(), "", "dev-int")
	if err != nil || len(only) != 1 || only[0].Target.IntegrationRef != "dev-int" {
		t.Fatalf("filtered check: %+v, %v", only, err)
	}
}

// Only an input error is refused: a name that starts with a dash, and a filter that names no recorded target. A store with no recorded target
// answers an empty list.
func TestCheckTargetRefusesOnlyInputErrors(t *testing.T) {
	k, _, _ := targetKit(t)
	ctx := context.Background()
	if got, err := k.sched.CheckRecordedTargets(ctx, "", ""); err != nil || len(got) != 0 {
		t.Fatalf("a store with no target: %+v, %v; want an empty answer", got, err)
	}
	k.recordTarget(t, "dev-int", "dev")
	for name, args := range map[string][2]string{
		"an integration ref starting with a dash": {"", "-dev-int"},
		"a checkout starting with a dash":         {"-C", ""},
		"an unknown integration ref":              {"", "nope-int"},
		"an unknown checkout":                     {"/not/a/recorded/checkout", ""},
	} {
		if got, err := k.sched.CheckRecordedTargets(ctx, args[0], args[1]); err == nil || got != nil || refusalReasonOf(err) != "merge_target_unreadable" {
			t.Errorf("%s: %+v, %v; want a merge_target_unreadable refusal", name, got, err)
		}
	}
	if faults := targetFaults(t, k); len(faults) != 0 || len(k.observations(t)) != 0 {
		t.Fatal("a refused check wrote something")
	}
}

// The check is not a step of the daemon: no file of the daemon package names it or reads a remote. The fault sweep stays free of network calls (docs/relay/faults.md).
func TestCheckTargetIsNotADaemonStep(t *testing.T) {
	files, err := filepath.Glob("../daemon/*.go")
	if err != nil || len(files) == 0 {
		t.Fatalf("daemon sources: %v, %v", files, err)
	}
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		raw, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, banned := range []string{"CheckRecordedTargets", "ls-remote", "integration_target"} {
			if strings.Contains(string(raw), banned) {
				t.Errorf("%s names %q", f, banned)
			}
		}
	}
}
