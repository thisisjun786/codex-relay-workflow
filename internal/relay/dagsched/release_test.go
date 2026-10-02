package dagsched

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/capacity"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/dag"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// Criterion c3, c11: a ready node is released to a child through the real managed engine, and the release leaves exactly the rows the protocol says: the intent, the
// frozen request, the manifest, a held slot, the bound execution. The node then reads as owned and its dependent keeps waiting.
func TestReleaseHappyPath(t *testing.T) {
	k := newReleaseKit(t)
	releasePlan(k.fixture, "rp")
	res := k.mustRelease("rp", "A")
	if !res.Bound || res.State != "admitted" || res.Replayed || res.RelationshipID == "" || res.ChildTaskID != "child-1" || res.Generation != 1 || res.SlotID == "" {
		t.Fatalf("result = %+v", res)
	}
	if created, sent := k.host.counts(); created != 1 || sent != 1 {
		t.Fatalf("host created %d sent %d, want 1 and 1", created, sent)
	}
	rows := k.rows()
	if rows != (rowCounts{releases: 1, requests: 1, manifests: 1, executions: 1, slots: 1}) {
		t.Fatalf("rows = %+v", rows)
	}
	if got := k.count("SELECT COUNT(*) FROM execution_slots WHERE subject_kind = 'dag_node' AND subject_key = 'rp/A' AND state = 'held'"); got != 1 {
		t.Fatal("the slot is not held under the node's subject")
	}
	if got := k.count("SELECT COUNT(*) FROM managed_start_requests WHERE request_id = ? AND state = 'attached'", ReleaseRequestID("A", res.ManifestDigest)); got != 1 {
		t.Fatal("the managed start is not attached under the derived request id")
	}
	reading := k.read("rp")
	if a := reading.node("A"); a.Reason != SkipAlreadyOwned || a.State != StateRunning {
		t.Fatalf("A = %+v", a)
	}
	if b := reading.node("B"); b.Reason != WaitEdge("ab") {
		t.Fatalf("B = %+v, want it waiting for A's acceptance", b)
	}
	// the business message carries the manifest and the verification instruction, in English
	message := k.host.sent[0]
	for _, want := range []string{res.ManifestDigest, "verify every uri, sha256 and byte count", "blocked_needs_input", "Implement the issue"} {
		if !strings.Contains(message, want) {
			t.Errorf("the child's assignment does not contain %q", want)
		}
	}
	// the stored manifest is the one the request carried, and the frozen request re-hashes
	body, found, err := k.repo.ReadManifest(context.Background(), res.ManifestDigest)
	if err != nil || !found || body["node_id"] != "A" || body["issue_key"] != "CRW-A" {
		t.Fatalf("manifest = %v %v %v", body, found, err)
	}
	var raw, sum string
	if err := k.s.DB.QueryRow("SELECT request_json, request_sha256 FROM dag_release_requests").Scan(&raw, &sum); err != nil || shaOf([]byte(raw)) != sum {
		t.Fatalf("frozen request: %v", err)
	}
	if !strings.Contains(raw, "\"requestId\":\""+res.RequestID+"\"") {
		t.Fatal("the frozen request does not carry the derived request id")
	}
}

// Criterion c3: a duplicate wake creates no second child and releases nothing wrongly, whether the calls come one after another or at once from two connections.
func TestReleaseDuplicateWake(t *testing.T) {
	k := newReleaseKit(t)
	releasePlan(k.fixture, "rp")
	first := k.mustRelease("rp", "A")
	for i := 0; i < 5; i++ {
		again := k.mustRelease("rp", "A")
		if !again.Replayed || !again.Bound || again.ChildTaskID != first.ChildTaskID || again.RelationshipID != first.RelationshipID {
			t.Fatalf("repeat %d = %+v", i, again)
		}
	}
	// a node that is not ready is refused and writes nothing
	before := k.rows()
	if _, err := k.release("rp", "B"); refusalReason(err) != "disposition_conflict" {
		t.Fatalf("release of a waiting node: %v", err)
	}
	if k.rows() != before {
		t.Fatal("a refused release wrote rows")
	}
	if created, _ := k.host.counts(); created != 1 || k.rows().releases != 1 || k.rows().slots != 1 {
		t.Fatalf("created %d, rows %+v", created, k.rows())
	}
}

// Criterion c3, c11 (the creation race): two connections call Release for one node at the same moment. Exactly one child exists and both calls end on it.
func TestReleaseCreationRace(t *testing.T) {
	k := newReleaseKit(t)
	releasePlan(k.fixture, "rp")
	second, err := store.Open(context.Background(), k.path, "")
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	other := &Scheduler{Store: second, Now: k.clock}
	k.wire(other)
	var wg sync.WaitGroup
	results := make([]ReleaseResult, 8)
	errs := make([]error, 8)
	for i := range results {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			s := k.sched
			if i%2 == 1 {
				s = other
			}
			snap := k.snapshot("rp")
			n, _ := nodeOf(snap, "A")
			results[i], errs[i] = s.Release(context.Background(), "rp", "A", "parent", k.request(n.Kind == dag.NodeImplementation))
		}(i)
	}
	wg.Wait()
	children := map[string]bool{}
	for i, res := range results {
		if errs[i] != nil {
			t.Fatalf("call %d: %v", i, errs[i])
		}
		if !res.Bound {
			t.Fatalf("call %d not bound: %+v", i, res)
		}
		children[res.ChildTaskID] = true
	}
	if created, _ := k.host.counts(); created != 1 || len(children) != 1 {
		t.Fatalf("created %d children, calls ended on %v", created, children)
	}
	if rows := k.rows(); rows.releases != 1 || rows.executions != 1 || rows.slots != 1 || rows.manifests != 1 {
		t.Fatalf("rows = %+v", rows)
	}
}

// Criterion c3 (lost creation response): the host created the child and the answer was lost. The node reads as creation_unknown with its slot held, and the next call binds the
// SAME child.
func TestReleaseLostCreationResponse(t *testing.T) {
	k := newReleaseKit(t)
	releasePlan(k.fixture, "rp")
	k.host.loseFirstCreation = true
	res, err := k.release("rp", "A")
	if err != nil {
		t.Fatal(err)
	}
	if res.Bound || res.State != "incomplete" || res.Reason != "creation_unknown" {
		t.Fatalf("first call = %+v", res)
	}
	if n := k.read("rp").node("A"); n.Reason != BlockedCreationUnknown || n.State != StateCreationUnknown {
		t.Fatalf("A after the lost response = %+v", n)
	}
	if k.rows().slots != 1 || k.rows().releases != 1 || k.rows().executions != 0 {
		t.Fatalf("rows = %+v", k.rows())
	}
	second := k.mustRelease("rp", "A")
	if !second.Bound || !second.Replayed || second.ChildTaskID != "child-1" {
		t.Fatalf("second call = %+v", second)
	}
	if created, _ := k.host.counts(); created != 1 {
		t.Fatalf("created %d threads, want the one that was already there", created)
	}
}

// E-22: the creation was accepted and the bind never ran. The next call replays the intent and binds the same child.
func TestReleaseBindFailureRecovers(t *testing.T) {
	k := newReleaseKit(t)
	releasePlan(k.fixture, "rp")
	failed := false
	k.sched.testAfterStart = func() error {
		if !failed {
			failed = true
			return context.DeadlineExceeded
		}
		return nil
	}
	if _, err := k.release("rp", "A"); err == nil {
		t.Fatal("the hook did not fail the first call")
	}
	if n := k.read("rp").node("A"); n.State != StateReleasing || n.Reason != SkipAlreadyOwned {
		t.Fatalf("A after the failed bind = %+v", n)
	}
	res := k.mustRelease("rp", "A")
	if !res.Bound || res.ChildTaskID != "child-1" {
		t.Fatalf("second call = %+v", res)
	}
	if created, _ := k.host.counts(); created != 1 {
		t.Fatalf("created %d", created)
	}
	if k.rows().executions != 1 {
		t.Fatalf("rows = %+v", k.rows())
	}
}

// A model, effort, sandbox or approval mismatch is refused (criterion c3): the engine compares what the host created with what was asked, and the release keeps its intent and
// its slot, binds nothing, and never creates a second child however often it is repeated.
func TestReleaseRefusesSettingsMismatch(t *testing.T) {
	for _, key := range []string{"model", "reasoningEffort", "sandbox", "approvalPolicy"} {
		t.Run(key, func(t *testing.T) {
			k := newReleaseKit(t)
			releasePlan(k.fixture, "rp")
			k.host.alterSetting = key
			for i := 0; i < 2; i++ {
				res, err := k.release("rp", "A")
				if err != nil {
					t.Fatal(err)
				}
				if res.Bound || res.State != "refused" || res.Reason != "creation_settings_unverified" {
					t.Fatalf("call %d = %+v", i, res)
				}
			}
			if created, sent := k.host.counts(); created != 1 || sent != 0 {
				t.Fatalf("created %d sent %d", created, sent)
			}
			if rows := k.rows(); rows.executions != 0 || rows.releases != 1 || rows.slots != 1 {
				t.Fatalf("rows = %+v", rows)
			}
		})
	}
}

// H1 (audit): a release whose manifest is no longer stored is not replayed.
func TestReleaseReplayWithoutStoredManifest(t *testing.T) {
	k := newReleaseKit(t)
	releasePlan(k.fixture, "rp")
	k.host.loseFirstCreation = true
	if res, err := k.release("rp", "A"); err != nil || res.Bound {
		t.Fatalf("first call = %v %+v", err, res)
	}
	k.exec("DELETE FROM dag_input_manifests")
	if _, err := k.release("rp", "A"); refusalReason(err) != "revision_mismatch" {
		t.Fatalf("replay with no manifest = %v", err)
	}
	if created, sent := k.host.counts(); created != 1 || sent != 0 {
		t.Fatalf("created %d sent %d", created, sent)
	}
}

// H2 (audit): a failure of the store while the slot is being reserved rolls the whole intent back. A slot that was written before the journal failed must not stay.
func TestReleaseStoreFailureDuringReservationRollsBack(t *testing.T) {
	k := newReleaseKit(t)
	releasePlan(k.fixture, "rp")
	k.exec("CREATE TRIGGER fail_journal BEFORE INSERT ON journal WHEN NEW.kind = 'slot_reserved' BEGIN SELECT RAISE(ABORT, 'the journal is full'); END")
	if _, err := k.release("rp", "A"); err == nil || !strings.Contains(err.Error(), "the journal is full") {
		t.Fatalf("release = %v", err)
	}
	if rows := k.rows(); rows != (rowCounts{}) {
		t.Fatalf("rows after the failure = %+v, want none (the slot was committed without its intent)", rows)
	}
	if created, _ := k.host.counts(); created != 0 {
		t.Fatal("a child was created")
	}
}

// H5 (audit): the slot of an intent whose child was never created was returned; replaying it must not create a child above the ceilings, and may re-reserve under them.
func TestReleaseReplayAfterTheSlotWasReturned(t *testing.T) {
	setup := func(t *testing.T) *releaseKit {
		k := newReleaseKit(t)
		releasePlan(k.fixture, "rp")
		k.host.loseFirstCreation = true
		if res, err := k.release("rp", "A"); err != nil || res.Bound {
			t.Fatalf("first call = %v %+v", err, res)
		}
		if _, err := (&capacity.Capacity{Store: k.s, Now: k.clock}).Release(context.Background(), capacity.Release{SubjectKind: SlotSubjectKind, SubjectKey: SlotSubjectKey("rp", "A"), ReleasedBy: "parent", Reason: "operator"}); err != nil {
			t.Fatal(err)
		}
		return k
	}
	t.Run("the ceilings are full", func(t *testing.T) {
		k := setup(t)
		k.holdSlots(6)
		_, sentBefore := k.host.counts()
		_, err := k.release("rp", "A")
		if refusalReason(err) != "capacity_exhausted" {
			t.Fatalf("replay = %v", err)
		}
		if _, sent := k.host.counts(); sent != sentBefore || k.count("SELECT COUNT(*) FROM dag_node_executions") != 0 {
			t.Fatal("a child was bound without a slot")
		}
	})
	t.Run("a slot is free again", func(t *testing.T) {
		k := setup(t)
		res, err := k.release("rp", "A")
		if err != nil || !res.Bound || res.SlotID == "" || res.ChildTaskID != "child-1" {
			t.Fatalf("replay = %v %+v", err, res)
		}
		if k.count("SELECT COUNT(*) FROM execution_slots WHERE subject_key = 'rp/A' AND state = 'held'") != 1 {
			t.Fatal("the slot was not reserved again")
		}
	})
}

// R2-H1 (audit): ids may contain a colon, so a slot key built with one would give two different nodes one slot, and the second release would read as the replay of the first.
func TestSlotKeysOfDifferentNodesNeverCollide(t *testing.T) {
	if SlotSubjectKey("a:b", "c") == SlotSubjectKey("a", "b:c") {
		t.Fatal("two nodes share a slot key")
	}
	k := newReleaseKit(t)
	k.putPlan("a:b", 0, "r1", addRelNode("c", dag.NodeNonPR))
	k.putPlan("a", 0, "r1", addRelNode("b:c", dag.NodeNonPR))
	for _, c := range [][2]string{{"a:b", "c"}, {"a", "b:c"}} {
		res, err := k.sched.Release(context.Background(), c[0], c[1], "parent", k.request(false))
		if err != nil || !res.Bound {
			t.Fatalf("release of %v = %v %+v", c, err, res)
		}
	}
	if got := k.count("SELECT COUNT(*) FROM execution_slots WHERE subject_kind = 'dag_node' AND state = 'held'"); got != 2 {
		t.Fatalf("%d held slots for two children, want 2", got)
	}
	if created, _ := k.host.counts(); created != 2 {
		t.Fatalf("created %d", created)
	}
}

// R3-H1 (audit): a managed start released before it created a child is a tombstone; replaying its release reserves no slot and starts nothing.
func TestReleaseReplayOfAReleasedRequestReclaimsNoSlot(t *testing.T) {
	k := newReleaseKit(t)
	releasePlan(k.fixture, "rp")
	started := 0
	real := k.sched.Start
	k.sched.Start = func(ctx context.Context, raw []byte) (StartAnswer, error) {
		started++
		if started == 1 {
			return StartAnswer{}, context.DeadlineExceeded // the engine never got as far as arming anything
		}
		return real(ctx, raw)
	}
	if _, err := k.release("rp", "A"); err == nil {
		t.Fatal("the first start was expected to fail")
	}
	var request string
	if err := k.s.DB.QueryRow("SELECT managed_request_id FROM dag_releases").Scan(&request); err != nil {
		t.Fatal(err)
	}
	k.managedRow(request, "CRW-A", "released", "")
	if _, err := (&capacity.Capacity{Store: k.s, Now: k.clock}).Release(context.Background(), capacity.Release{SubjectKind: SlotSubjectKind, SubjectKey: SlotSubjectKey("rp", "A"), ReleasedBy: "parent", Reason: "operator"}); err != nil {
		t.Fatal(err)
	}
	_, err := k.release("rp", "A")
	if refusalReason(err) != "disposition_conflict" || !strings.Contains(err.Error(), BlockedReleaseAbandoned) {
		t.Fatalf("replay = %v", err)
	}
	if started != 1 || k.count("SELECT COUNT(*) FROM execution_slots WHERE subject_kind = 'dag_node' AND state = 'held'") != 0 {
		t.Fatalf("started %d, held slots %d: a tombstone reclaimed a slot", started, k.count("SELECT COUNT(*) FROM execution_slots WHERE state = 'held'"))
	}
}

// R3-H2 (audit): a paused parent still owns its project and still sits under the initiative's ceiling. The reader and the replay must apply the standing cap to the initiative exactly where
// capacity.Reserve counts it, or a replay is the way around the cap.
func TestInitiativeClampAppliesWhileTheParentIsPaused(t *testing.T) {
	k := newReleaseKit(t)
	releasePlan(k.fixture, "rp")
	k.initiativeAbove("INIT-1")
	k.declareLimit("initiative", "INIT-1", "runs", 20)
	k.holdSlotsOf(6, "OTHER", "INIT-1")
	k.host.loseFirstCreation = true
	// before anything is held the first release is judged under the clamp too
	for _, status := range []string{"active", "paused"} {
		k.exec("UPDATE scope_bindings SET status = ? WHERE binding_id = 'bind-parent'", status)
		if c := k.capacityNow(); c.Ceiling != 6 || c.Source != "clamped_no_basis" || c.Free != 0 {
			t.Fatalf("parent %s: capacity = %+v, want the initiative's 20 clamped to 6 with 6 held by another project", status, c)
		}
	}
	// the replay of an unbound intent whose slot was returned
	k.exec("UPDATE scope_bindings SET status = 'active' WHERE binding_id = 'bind-parent'")
	k.exec("DELETE FROM execution_slots WHERE subject_key LIKE 'other-%'")
	if res, err := k.release("rp", "A"); err != nil || res.Bound {
		t.Fatalf("first call = %v %+v", err, res)
	}
	if _, err := (&capacity.Capacity{Store: k.s, Now: k.clock}).Release(context.Background(), capacity.Release{SubjectKind: SlotSubjectKind, SubjectKey: SlotSubjectKey("rp", "A"), ReleasedBy: "parent", Reason: "operator"}); err != nil {
		t.Fatal(err)
	}
	k.holdSlotsOf(6, "OTHER", "INIT-1")
	k.exec("UPDATE scope_bindings SET status = 'paused' WHERE binding_id = 'bind-parent'")
	if _, err := k.release("rp", "A"); refusalReason(err) != "capacity_exhausted" {
		t.Fatalf("replay with a paused parent above a full initiative = %v", err)
	}
	if k.count("SELECT COUNT(*) FROM execution_slots WHERE subject_key = ? AND state = 'held'", SlotSubjectKey("rp", "A")) != 0 {
		t.Fatal("the replay reserved a slot above the initiative's cap")
	}
}
