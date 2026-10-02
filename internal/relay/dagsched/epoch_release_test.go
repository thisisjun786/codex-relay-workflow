package dagsched

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport/storeseed"
)

// bindParent registers the parent of another project.
func (k *releaseKit) bindParent(id, task, project string) {
	k.t.Helper()
	now := k.clock()
	if err := storeseed.InsertScopeBinding(context.Background(), k.s, store.ScopeBindingsRow{BindingID: id, Role: "parent", ScopeKind: "project", ScopeKey: project, TaskID: task, HostID: "host",
		Status: "active", Revision: 1, CreatedAt: now, UpdatedAt: now}); err != nil {
		k.t.Fatal(err)
	}
}

// The fence of a release is checked again where the release writes. A session that holds the epoch when Release starts but loses it before a transaction of the release writes is refused
// there, and what had already happened (a child the host created) is the only thing it left. Each boundary is driven by a claim that lands exactly there.

func TestAClaimBetweenTheStartOfAReleaseAndItsIntentRefusesTheIntent(t *testing.T) {
	k := newReleaseKit(t)
	releasePlan(k.fixture, "rp")
	stale := k.sched
	k.claim(stale, "rp", "parent", "session-1")
	stale.testBetweenReadAndIntent = func() { k.session("rp", "parent", "session-2") }
	before := allRows(t, k.s.DB)
	if _, err := stale.Release(context.Background(), "rp", "A", "parent", k.request(false)); !isStale(err) {
		t.Fatalf("release = %v, want stale_coordinator_epoch", err)
	}
	after := allRows(t, k.s.DB)
	delete(before, "dag_coordinator_claims")
	delete(after, "dag_coordinator_claims")
	if !reflect.DeepEqual(before, after) {
		t.Fatal("the stale release wrote something before its intent was refused")
	}
	if created, _ := k.host.counts(); created != 0 {
		t.Fatalf("%d children", created)
	}
}

func TestAClaimBeforeTheReplayTransactionOfAReleaseRefusesTheReplay(t *testing.T) {
	k := newReleaseKit(t)
	releasePlan(k.fixture, "rp")
	stale := k.sched
	k.claim(stale, "rp", "parent", "session-1")
	// an intent that never reached the host
	real := stale.Start
	stale.Start = func(context.Context, []byte) (StartAnswer, error) { return StartAnswer{}, errCrash }
	if _, err := stale.Release(context.Background(), "rp", "A", "parent", k.request(false)); !errors.Is(err, errCrash) {
		t.Fatalf("the intent was not written: %v", err)
	}
	stale.Start = real
	stale.testBeforeReplayTx = func() { k.session("rp", "parent", "session-2") }
	before := allRows(t, k.s.DB)
	if _, err := stale.Release(context.Background(), "rp", "A", "parent", k.request(false)); !isStale(err) {
		t.Fatalf("the replay = %v, want stale_coordinator_epoch", err)
	}
	after := allRows(t, k.s.DB)
	delete(before, "dag_coordinator_claims")
	delete(after, "dag_coordinator_claims")
	if !reflect.DeepEqual(before, after) {
		t.Fatal("the stale replay wrote something")
	}
	if created, _ := k.host.counts(); created != 0 {
		t.Fatalf("%d children: the stale session started the managed start", created)
	}
}

func TestAClaimAfterTheManagedStartRefusesTheBindAndTheNewSessionBindsTheSameChild(t *testing.T) {
	k := newReleaseKit(t)
	releasePlan(k.fixture, "rp")
	stale := k.sched
	k.claim(stale, "rp", "parent", "session-1")
	var second *Scheduler
	stale.testAfterStart = func() error {
		second = k.session("rp", "parent", "session-2")
		return nil
	}
	if _, err := stale.Release(context.Background(), "rp", "A", "parent", k.request(false)); !isStale(err) {
		t.Fatalf("the bind = %v, want stale_coordinator_epoch", err)
	}
	if created, _ := k.host.counts(); created != 1 {
		t.Fatalf("%d children: the managed start ran once and the host holds that child", created)
	}
	if k.count("SELECT COUNT(*) FROM dag_node_executions") != 0 {
		t.Fatal("the stale session bound the child")
	}
	res, err := second.Release(context.Background(), "rp", "A", "parent", k.request(false))
	if err != nil || !res.Bound || !res.Replayed || res.ChildTaskID != "child-1" {
		t.Fatalf("the new session = %+v, %v", res, err)
	}
	if created, _ := k.host.counts(); created != 1 || k.count("SELECT COUNT(*) FROM dag_node_executions") != 1 {
		t.Fatalf("created %d, executions %d", created, k.count("SELECT COUNT(*) FROM dag_node_executions"))
	}
}

// The stale_head row a release appends when the forge shows a moved head is a write of that release: a session that lost the epoch while the forge was read appends nothing and is told it is stale.
func TestAClaimWhileAPinnedPredecessorIsReadRefusesTheStaleHeadRow(t *testing.T) {
	k := newReleaseKit(t)
	releasePlan(k.fixture, "rp")
	stale := k.sched
	k.claim(stale, "rp", "parent", "session-1")
	k.pinned()
	moved := "2222222222222222222222222222222222222222"
	k.forge.by["owner/repo#7"] = openPR("owner/repo", 7, moved)
	k.forge.onRead = func() { k.session("rp", "parent", "session-2") }
	if _, err := stale.Release(context.Background(), "rp", "J", "parent", k.request(true)); !isStale(err) {
		t.Fatalf("release = %v, want stale_coordinator_epoch and not merge_candidate_moved", err)
	}
	if n := k.count("SELECT COUNT(*) FROM dag_merge_checks"); n != 0 {
		t.Fatalf("%d merge check rows: the stale session recorded what it read", n)
	}
}

// Closing an abandoned release lets the node be released again, so it is a decision: a stale session cannot close one, and the session that holds the epoch can.
func TestAStaleSessionCannotCloseAnAbandonedRelease(t *testing.T) {
	k := newReleaseKit(t)
	releasePlan(k.fixture, "rp")
	stale := k.sched
	k.claim(stale, "rp", "parent", "session-1")
	digest, request := k.abandon("rp", "A")
	current := k.session("rp", "parent", "session-2")
	before := allRows(t, k.s.DB)
	if _, err := stale.CloseRelease(context.Background(), "rp", "A", "parent", digest, "the operator released the start", request); !isStale(err) {
		t.Fatalf("close by the stale session = %v", err)
	}
	if !equalRows(before, allRows(t, k.s.DB)) {
		t.Fatal("a refused close changed the store")
	}
	if _, err := current.CloseRelease(context.Background(), "rp", "A", "parent", digest, "the operator released the start", request); err != nil {
		t.Fatalf("close by the session that holds the epoch: %v", err)
	}
	var epoch int64
	if err := k.s.DB.QueryRow("SELECT coordinator_epoch FROM dag_release_recoveries WHERE action = 'closed'").Scan(&epoch); err != nil || epoch != 2 {
		t.Fatalf("the closure carries epoch %d (%v), want 2", epoch, err)
	}
}

// A plan that has no revision belongs to the project it was claimed for: a parent of another project does not take it over before its first revision, and a replacement parent of the first project does.
func TestAHeaderlessPlanStaysWithTheProjectItWasClaimedFor(t *testing.T) {
	ctx := context.Background()
	k := newReleaseKit(t)
	if _, err := k.sched.ClaimEpoch(ctx, "fresh", ClaimInput{Actor: "parent", SessionNonce: "s1", Project: "P-TEST"}); err != nil {
		t.Fatal(err)
	}
	k.bindParent("bind-other", "other-parent", "P-OTHER")
	if _, err := k.sched.ClaimEpoch(ctx, "fresh", ClaimInput{Actor: "other-parent", SessionNonce: "s2", Project: "P-OTHER"}); refusalReason(err) != "malformed_receipt" {
		t.Fatalf("a parent of another project claiming a plan claimed for P-TEST = %v", err)
	}
	if _, err := k.sched.ClaimEpoch(ctx, "fresh", ClaimInput{Actor: "other-parent", SessionNonce: "s2"}); refusalReason(err) != "scope_role_mismatch" {
		t.Fatalf("the same without naming a project = %v", err)
	}
	if k.count("SELECT COUNT(*) FROM dag_coordinator_claims WHERE plan_id = 'fresh'") != 1 {
		t.Fatal("a refused claim wrote a row")
	}
	// the project is the claim's: the replacement parent of P-TEST claims without naming it
	k.replaceParent("parent-2")
	if res, err := k.sched.ClaimEpoch(ctx, "fresh", ClaimInput{Actor: "parent-2", SessionNonce: "s3"}); err != nil || res.Epoch != 2 || res.ProjectKey != "P-TEST" {
		t.Fatalf("the replacement parent's claim = %+v, %v", res, err)
	}
}
