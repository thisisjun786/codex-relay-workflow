package dagsched

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// CRW-965 (parent decision, D5): a remote that answers ls-remote and rejects the push with an HTTP 503 status text is an
// outage: the push is deferred, the acceptance, integration, observation and integrated count are unaffected, and the same
// push completes once the remote accepts it.
func TestServerErrorOnPushDefersOnlyThePush(t *testing.T) {
	k := newBatchKit(t, batchNode{name: "a", files: map[string]string{"a.txt": "a\n"}}, batchNode{name: "b", files: map[string]string{"b.txt": "b\n"}})
	remote := bareRemote(t)
	k.pushRepo(t, remote)
	hook := filepath.Join(remote, "hooks", "pre-receive")
	if err := os.WriteFile(hook, []byte("#!/bin/sh\necho 'error: The requested URL returned error: 503' >&2\nexit 1\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	k.acceptByCommit("a")
	k.acceptByCommit("b")
	ctx := context.Background()
	res, err := k.sched.IntegrateBatch(ctx, k.batchIn(), IntegrationBatchDeps{Verify: stubVerifier(writeStubVerifier(t)), Update: updateIntegrationRef})
	if err != nil || len(res.Merged) != 2 {
		t.Fatalf("batch while the remote answers 503: %+v, %v", res, err)
	}
	k.sched.Tips = newLegacyLocalTipReader(t)
	k.sched.Ancestry = GitAncestry{}.Ancestry
	for _, node := range []string{"a", "b"} {
		obs, err := k.sched.ObserveIntegration(ctx, "g", node, "parent", []Target{{Repository: k.repo.path, BaseRef: "dev-int"}})
		if err != nil || !obs.Integrated {
			t.Fatalf("observe %s during the outage: %+v, %v; want integrated", node, obs, err)
		}
	}
	progress, err := k.sched.ReadProgress(ctx, "g")
	if err != nil || progress.Cumulative.Integrated.Nodes != 2 {
		t.Fatalf("integrated count during the outage: %v, %v; want 2", progress.Cumulative.Integrated.Nodes, err)
	}
	deferred, err := PushIntegration(ctx, k.repo.path, "origin", "dev", "dev-int", k.sched.VerifiedMoveOnto, nil)
	if err != nil || deferred.Outcome != PushDeferred {
		t.Fatalf("push answered 503: %+v, %v; want deferred", deferred, err)
	}
	if got := remoteBranch(t, remote, "dev"); got != "" {
		t.Fatalf("the remote dev was written while the remote answered 503: %s", got)
	}
	if err := os.Remove(hook); err != nil {
		t.Fatal(err)
	}
	done, err := PushIntegration(ctx, k.repo.path, "origin", "dev", "dev-int", k.sched.VerifiedMoveOnto, nil)
	if err != nil || done.Outcome != PushPushed {
		t.Fatalf("push after the remote accepts again: %+v, %v; want pushed", done, err)
	}
	if got := remoteBranch(t, remote, "dev"); got != res.NewHead {
		t.Fatalf("remote dev is %s; want %s", got, res.NewHead)
	}
}
