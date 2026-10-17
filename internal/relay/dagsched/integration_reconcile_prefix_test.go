package dagsched

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

func TestCompletedBatchesAreNotAbandonedByLaterBatches(t *testing.T) {
	t.Parallel()
	k := newBatchKit(t,
		batchNode{name: "a", files: map[string]string{"a.txt": "a"}},
		batchNode{name: "b", files: map[string]string{"b.txt": "b"}},
		batchNode{name: "c", files: map[string]string{"c.txt": "c"}})
	in := k.batchIn()
	deps := IntegrationDeps(stubVerifier(writeStubVerifier(t)))
	var completed []string
	for _, node := range []string{"a", "b", "c"} {
		k.acceptByCommit(node)
		in.Nodes = []string{node}
		res, err := k.sched.IntegrateBatch(context.Background(), in, deps)
		if err != nil {
			t.Fatalf("batch %s: %v", node, err)
		}
		if len(res.Merged) != 1 || res.Merged[0].NodeID != node || len(res.MarkedEvents) != 1 {
			t.Fatalf("batch %s: merged=%v marked=%v", node, res.Merged, res.MarkedEvents)
		}
		if len(res.Abandoned) != 0 {
			t.Errorf("batch %s abandoned completed batches: %v; completed=%v", node, res.Abandoned, completed)
		}
		completed = append(completed, res.BatchID)
		var moved, marked int
		for _, row := range k.stageRows() {
			if row.BatchID != res.BatchID {
				continue
			}
			if row.Stage == "ref_moved" && row.Detail == res.NewHead {
				moved++
			}
			if row.Stage == "marked" {
				marked++
			}
		}
		if moved != 1 || marked != 1 {
			t.Fatalf("batch %s: ref_moved=%d marked=%d; want one each", node, moved, marked)
		}
	}
}

func TestUnmovedBatchIsAbandonedWhenVerifiedHeadIsNotTip(t *testing.T) {
	t.Parallel()
	k := newBatchKit(t, batchNode{name: "a", files: map[string]string{"a.txt": "a"}})
	k.acceptByCommit("a")
	in := k.batchIn()
	k.repo.git("branch", in.IntegrationRef, k.base)
	deps := IntegrationDeps(stubVerifier(writeStubVerifier(t)))
	deps.Update = func(context.Context, string, string, string, string) error {
		return fmt.Errorf("stop before moving the ref")
	}
	res, err := k.sched.IntegrateBatch(context.Background(), in, deps)
	if err == nil || len(verifiedIntents(t, k)) != 1 || countStage(k.stageRows(), "ref_moved") != 0 {
		t.Fatalf("want a verified intent without a move: %+v, %v", res, err)
	}
	tip, found := k.branchTip(in.IntegrationRef)
	if !found || tip != k.base {
		t.Fatalf("branch tip=%s found=%v; want unchanged base %s", tip, found, k.base)
	}
	var abandoned IntegrationBatchResult
	if err := k.sched.reconcilePlannedMoves(context.Background(), in, tip, &abandoned); err != nil || len(abandoned.Reconciled) != 0 || len(abandoned.Abandoned) != 1 || abandoned.Abandoned[0] != res.BatchID {
		t.Errorf("unmoved batch must be abandoned: %+v, %v", abandoned, err)
	}
}

// The command for b calls back before merging, after a's verified intent is
// durable. This fixes the interleaving without sleeps or direct database edits.
func TestCommandPrefixRecoveryAfterEarlierPrefixReconciled(t *testing.T) {
	k := newBatchKit(t,
		batchNode{name: "a", files: map[string]string{"a.txt": "a"}},
		batchNode{name: "b", files: map[string]string{"b.txt": "b"}},
		batchNode{name: "c", files: map[string]string{"c.txt": "c"}})
	remote := bareRemote(t)
	k.pushRepo(t, remote)
	for _, node := range []string{"a", "b", "c"} {
		k.acceptByCommit(node)
	}
	in := k.batchIn()
	// Repository integration commands currently run only against dev.
	in.IntegrationRef = "dev"
	ctx := context.Background()
	type recovery struct {
		head   string
		result IntegrationBatchResult
		err    error
	}
	recovered := make(chan recovery, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		head, found, err := integrationBranchTip(r.Context(), k.repo.path, in.IntegrationRef)
		var result IntegrationBatchResult
		if err == nil && !found {
			err = fmt.Errorf("integration branch is missing")
		}
		if err == nil {
			err = k.sched.reconcilePlannedMoves(r.Context(), in, head, &result)
		}
		recovered <- recovery{head, result, err}
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
	}))
	defer server.Close()
	bin, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	quote := func(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }
	command := commandPrefixScript(t, "set -eu\n"+
		"if git cat-file -e \"$CRW_INTEGRATE_HEAD:c.txt\" 2>/dev/null; then exit 9; fi\n"+
		"if git cat-file -e \"$CRW_INTEGRATE_HEAD:b.txt\" 2>/dev/null; then\n"+
		"  CRW_TEST_RECONCILE_URL="+quote(server.URL)+" "+quote(bin)+" -test.run='^TestCommandPrefixReconcileHelper$' -test.count=1\n"+
		"fi\ngit merge --no-ff --no-edit -m command-merge \"$1\"\n")
	commandConfig(t, k.repo.path, "true", command)
	first, err := k.sched.IntegrateBatch(ctx, in, IntegrationDeps(stubVerifier(writeStubVerifier(t))))
	if err == nil || !strings.Contains(err.Error(), "integrate_command exited 9") {
		t.Fatalf("expected c's command failure, got %v", err)
	}
	var earlier recovery
	select {
	case earlier = <-recovered:
	default:
		t.Fatal("b never waited for the other call to reconcile a")
	}
	if earlier.err != nil || len(earlier.result.Reconciled) != 1 || earlier.result.Reconciled[0] != first.BatchID {
		t.Fatalf("earlier prefix recovery: %+v", earlier)
	}
	tip := k.repo.git("rev-parse", "dev")
	if earlier.head != k.repo.git("rev-parse", tip+"^1") {
		t.Fatalf("earlier recovery head %s is not a's prefix before %s", earlier.head, tip)
	}
	intents := verifiedIntents(t, k)
	if len(intents) != 2 {
		t.Fatalf("verified prefixes = %v; want a and a+b", intents)
	}
	for _, intent := range intents {
		if intent["_batch"] != first.BatchID || (intent["verified_head"] != earlier.head && intent["verified_head"] != tip) {
			t.Fatalf("unexpected verified prefix: %v", intent)
		}
	}
	if ok, err := k.sched.VerifiedMoveOnto(ctx, in.IntegrationRef, earlier.head); err != nil || !ok {
		t.Fatalf("a was not recorded as moved: %v, %v", ok, err)
	}
	if ok, err := k.sched.VerifiedMoveOnto(ctx, in.IntegrationRef, tip); err != nil || ok {
		t.Fatalf("a+b was recorded as moved before recovery: %v, %v", ok, err)
	}
	in.Nodes = []string{"a", "b"}
	res, err := k.sched.IntegrateBatch(ctx, in, IntegrationDeps(stubVerifier(writeStubVerifier(t))))
	if err != nil || len(res.MarkedEvents) != 2 || len(res.ContainedUnverified) != 0 {
		t.Fatalf("retry: marked=%v contained=%v err=%v", res.MarkedEvents, res.ContainedUnverified, err)
	}
	if ok, err := k.sched.VerifiedMoveOnto(ctx, in.IntegrationRef, tip); err != nil || !ok {
		t.Errorf("latest verified prefix is not a recorded move after retry: %v, %v", ok, err)
	}
	push, err := PushIntegration(ctx, k.repo.path, "origin", "dev", in.IntegrationRef, k.sched.VerifiedMoveOnto, nil)
	if err != nil || push.Outcome != PushPushed {
		t.Errorf("recovered prefix must push: %+v, %v", push, err)
	}
	if got := remoteBranch(t, remote, "dev"); got != tip {
		t.Errorf("remote dev = %s; want recovered prefix %s", got, tip)
	}
	if len(res.Reconciled) != 1 || res.Reconciled[0] != first.BatchID || len(res.Abandoned) != 0 {
		t.Errorf("retry reconciled=%v abandoned=%v", res.Reconciled, res.Abandoned)
	}
	var repeated IntegrationBatchResult
	if err := k.sched.reconcilePlannedMoves(ctx, in, tip, &repeated); err != nil || len(repeated.Reconciled) != 0 || len(repeated.Abandoned) != 0 {
		t.Errorf("repeated recovery: %+v, %v", repeated, err)
	}
	moves := map[string]int{}
	for _, row := range k.stageRows() {
		if row.BatchID == first.BatchID && row.Stage == "ref_moved" {
			moves[row.Detail]++
		}
	}
	if len(moves) != 2 || moves[earlier.head] != 1 || moves[tip] != 1 {
		t.Errorf("move rows by head = %v; want a and a+b once each", moves)
	}
	var abandoned IntegrationBatchResult
	if err := k.sched.reconcilePlannedMoves(ctx, in, k.base, &abandoned); err != nil || len(abandoned.Reconciled) != 0 || len(abandoned.Abandoned) != 0 {
		t.Errorf("recorded moves must keep the batch out of abandoned: %+v, %v", abandoned, err)
	}
}

func TestCommandPrefixReconcileHelper(t *testing.T) {
	url := os.Getenv("CRW_TEST_RECONCILE_URL")
	if url == "" {
		return
	}
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("reconciliation returned %s", resp.Status)
	}
}
