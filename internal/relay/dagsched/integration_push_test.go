package dagsched

import (
	"context"
	"os/exec"
	"strings"
	"testing"
)

// The push of the integration branch (CRW-965, c6 and c7), against a temporary bare remote. A GitHub outage is
// simulated with a push URL that refuses connections while the fetch URL, which ls-remote reads, still works.

// bareRemote is a temporary bare repository the push tests use as origin.
func bareRemote(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if out, err := exec.Command("git", "init", "-q", "--bare", dir).CombinedOutput(); err != nil {
		t.Fatalf("git init --bare: %v: %s", err, out)
	}
	return dir
}

// remoteBranch reads the commit a bare remote's branch points at, or "" when it has none.
func remoteBranch(t *testing.T, remote, branch string) string {
	t.Helper()
	out, err := exec.Command("git", "-C", remote, "rev-parse", "--verify", "-q", "refs/heads/"+branch).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// pushRepo is a checkout of the kit with origin pointing at the bare remote.
func (k *batchKit) pushRepo(t *testing.T, remote string) {
	t.Helper()
	k.repo.git("remote", "add", "origin", remote)
}

// TestPushFastForwardsTheSameCommit: the remote branch takes the integration commit by a fast-forward, and a second
// push is up to date (c7).
func TestPushFastForwardsTheSameCommit(t *testing.T) {
	k := newBatchKit(t, batchNode{name: "a", files: map[string]string{"a.txt": "a\n"}})
	remote := bareRemote(t)
	k.pushRepo(t, remote)
	k.acceptByCommit("a")
	res, err := k.sched.IntegrateBatch(context.Background(), k.batchIn(), IntegrationBatchDeps{Verify: stubVerifier(writeStubVerifier(t)), Update: updateIntegrationRef})
	if err != nil {
		t.Fatalf("batch: %v", err)
	}
	first, err := PushIntegration(context.Background(), k.repo.path, "origin", "dev", "dev-int")
	if err != nil || first.Outcome != PushPushed {
		t.Fatalf("first push: %+v, %v", first, err)
	}
	if got := remoteBranch(t, remote, "dev"); got != res.NewHead {
		t.Fatalf("remote dev is %s; want the integration commit %s", got, res.NewHead)
	}
	second, err := PushIntegration(context.Background(), k.repo.path, "origin", "dev", "dev-int")
	if err != nil || second.Outcome != PushUpToDate {
		t.Fatalf("second push: %+v, %v; want up_to_date", second, err)
	}
}

// c7: the remote dev points at a commit the integration branch does not contain. Nothing is pushed, the command reports
// merge_base_mismatch, and the remote keeps its commit.
func TestPushRefusesToOverwriteARemoteThatHasMoved(t *testing.T) {
	k := newBatchKit(t, batchNode{name: "a", files: map[string]string{"a.txt": "a\n"}})
	remote := bareRemote(t)
	k.pushRepo(t, remote)
	k.acceptByCommit("a")
	if _, err := k.sched.IntegrateBatch(context.Background(), k.batchIn(), IntegrationBatchDeps{Verify: stubVerifier(writeStubVerifier(t)), Update: updateIntegrationRef}); err != nil {
		t.Fatalf("batch: %v", err)
	}
	k.repo.git("checkout", "-q", "-B", "elsewhere", k.base)
	k.repo.write("other.txt", "other\n")
	k.repo.git("add", "other.txt")
	k.repo.git("commit", "-q", "-m", "other")
	other := k.repo.git("rev-parse", "HEAD")
	k.repo.git("push", "-q", remote, other+":refs/heads/dev")
	_, err := PushIntegration(context.Background(), k.repo.path, "origin", "dev", "dev-int")
	if refusalReasonOf(err) != "merge_base_mismatch" {
		t.Fatalf("divergent push: %v; want merge_base_mismatch", err)
	}
	if got := remoteBranch(t, remote, "dev"); got != other {
		t.Fatalf("the remote dev moved to %s; it must keep %s", got, other)
	}
}

// c6: a GitHub outage. ls-remote reads the remote, the push cannot reach it: the push is deferred, the integration the
// batch recorded stands, the acceptance and the integrated count go through, and the same push completes later.
func TestOutageDefersOnlyThePushAndTheIntegrationCompletes(t *testing.T) {
	k := newBatchKit(t, batchNode{name: "a", files: map[string]string{"a.txt": "a\n"}}, batchNode{name: "b", files: map[string]string{"b.txt": "b\n"}})
	remote := bareRemote(t)
	k.pushRepo(t, remote)
	k.repo.git("config", "remote.origin.pushurl", "http://127.0.0.1:1/codex-relay-workflow.git")
	k.acceptByCommit("a")
	k.acceptByCommit("b")
	res, err := k.sched.IntegrateBatch(context.Background(), k.batchIn(), IntegrationBatchDeps{Verify: stubVerifier(writeStubVerifier(t)), Update: updateIntegrationRef})
	if err != nil || len(res.Merged) != 2 {
		t.Fatalf("batch during the outage: %+v, %v", res, err)
	}
	// integration is observed on the local integration branch, the target the node has no edge for
	k.sched.Tips = newLegacyLocalTipReader(t)
	k.sched.Ancestry = GitAncestry{}.Ancestry
	for _, node := range []string{"a", "b"} {
		obs, err := k.sched.ObserveIntegration(context.Background(), "g", node, "parent", []Target{{Repository: k.repo.path, BaseRef: "dev-int"}})
		if err != nil || !obs.Integrated {
			t.Fatalf("observe %s on the integration branch: %+v, %v; want integrated", node, obs, err)
		}
	}
	progress, err := k.sched.ReadProgress(context.Background(), "g")
	if err != nil {
		t.Fatal(err)
	}
	if progress.Cumulative.Integrated.Nodes != 2 {
		t.Fatalf("integrated count %d during the outage; want 2", progress.Cumulative.Integrated.Nodes)
	}
	deferred, err := PushIntegration(context.Background(), k.repo.path, "origin", "dev", "dev-int")
	if err != nil || deferred.Outcome != PushDeferred {
		t.Fatalf("push during the outage: %+v, %v; want deferred", deferred, err)
	}
	if got := remoteBranch(t, remote, "dev"); got != "" {
		t.Fatalf("the remote dev was written during the outage: %s", got)
	}
	// the outage ends: the same push completes, and the integrated count is unchanged
	k.repo.git("config", "--unset", "remote.origin.pushurl")
	done, err := PushIntegration(context.Background(), k.repo.path, "origin", "dev", "dev-int")
	if err != nil || done.Outcome != PushPushed {
		t.Fatalf("push after the outage: %+v, %v; want pushed", done, err)
	}
	if got := remoteBranch(t, remote, "dev"); got != res.NewHead {
		t.Fatalf("remote dev is %s; want %s", got, res.NewHead)
	}
	after, err := k.sched.ReadProgress(context.Background(), "g")
	if err != nil || after.Cumulative.Integrated.Nodes != 2 {
		t.Fatalf("integrated count after the push: %v, %v; want 2", after.Cumulative.Integrated.Nodes, err)
	}
}
