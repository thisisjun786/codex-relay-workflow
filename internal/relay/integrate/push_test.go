package integrate

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// pushFixture is a temporary bare remote and a local checkout whose integration branch starts at base.
type pushFixture struct {
	repo   settleRepo
	remote string
}

func newPushFixture(t *testing.T) pushFixture {
	t.Helper()
	repo := newSettleRepo(t)
	remote := filepath.Join(t.TempDir(), "remote.git")
	gitIn(t, repo.dir, "init", "-q", "--bare", remote)
	gitIn(t, repo.dir, "branch", "-f", "dev-int", repo.base)
	return pushFixture{repo: repo, remote: remote}
}

// advance commits one more file on the integration branch and returns its new head.
func (f pushFixture) advance(t *testing.T, name string) string {
	t.Helper()
	gitIn(t, f.repo.dir, "checkout", "-q", "dev-int")
	writeFile(t, f.repo.dir, name, name+"\n")
	gitIn(t, f.repo.dir, "add", name)
	gitIn(t, f.repo.dir, "commit", "-q", "-m", name)
	return gitIn(t, f.repo.dir, "rev-parse", "HEAD")
}

func remoteDev(t *testing.T, f pushFixture) string {
	t.Helper()
	out := gitIn(t, f.repo.dir, "ls-remote", f.remote, "refs/heads/dev")
	if out == "" {
		return ""
	}
	return out[:40]
}

func refusalReason(err error) string {
	var refused *store.RefusedError
	if errors.As(err, &refused) {
		return refused.Reason
	}
	return ""
}

// A fast-forward: the remote dev holds an ancestor of the integration head, so the push advances it.
func TestPushFastForwardsTheSameCommit(t *testing.T) {
	f := newPushFixture(t)
	first := f.advance(t, "one.txt")
	res, err := PushIntegration(context.Background(), f.repo.dir, f.remote, "dev", "dev-int")
	if err != nil || res.Outcome != PushPushed {
		t.Fatalf("first push: %+v, %v", res, err)
	}
	if got := remoteDev(t, f); got != first {
		t.Fatalf("remote dev is %s; want the integration head %s", got, first)
	}
	second := f.advance(t, "two.txt")
	res, err = PushIntegration(context.Background(), f.repo.dir, f.remote, "dev", "dev-int")
	if err != nil || res.Outcome != PushPushed || remoteDev(t, f) != second {
		t.Fatalf("fast-forward: %+v, %v; remote %s", res, err, remoteDev(t, f))
	}
}

func TestPushIsUpToDateWhenTheRemoteHoldsTheHead(t *testing.T) {
	f := newPushFixture(t)
	f.advance(t, "one.txt")
	if _, err := PushIntegration(context.Background(), f.repo.dir, f.remote, "dev", "dev-int"); err != nil {
		t.Fatal(err)
	}
	res, err := PushIntegration(context.Background(), f.repo.dir, f.remote, "dev", "dev-int")
	if err != nil || res.Outcome != PushUpToDate {
		t.Fatalf("second push: %+v, %v; want up_to_date", res, err)
	}
}

// c7: the remote dev points at a commit the integration branch does not contain. Nothing is pushed and the
// command reports it; the remote keeps its commit.
func TestPushRefusesToOverwriteARemoteThatHasMoved(t *testing.T) {
	f := newPushFixture(t)
	f.advance(t, "one.txt")
	// another writer pushes a commit the integration branch does not have
	gitIn(t, f.repo.dir, "checkout", "-q", "-B", "elsewhere", f.repo.base)
	writeFile(t, f.repo.dir, "other.txt", "other\n")
	gitIn(t, f.repo.dir, "add", "other.txt")
	gitIn(t, f.repo.dir, "commit", "-q", "-m", "other")
	other := gitIn(t, f.repo.dir, "rev-parse", "HEAD")
	gitIn(t, f.repo.dir, "push", "-q", f.remote, other+":refs/heads/dev")
	res, err := PushIntegration(context.Background(), f.repo.dir, f.remote, "dev", "dev-int")
	if refusalReason(err) != string(contract.RefusalMergeBaseMismatch) {
		t.Fatalf("divergent push: %+v, %v; want merge_base_mismatch", res, err)
	}
	if got := remoteDev(t, f); got != other {
		t.Fatalf("the remote dev moved to %s; it must keep %s", got, other)
	}
}

// c6: with the remote unreachable (a GitHub outage) the push is deferred and the local integration stands.
func TestPushDefersWhenTheRemoteIsUnreachable(t *testing.T) {
	f := newPushFixture(t)
	head := f.advance(t, "one.txt")
	res, err := PushIntegration(context.Background(), f.repo.dir, "http://127.0.0.1:1/codex-relay-workflow.git", "dev", "dev-int")
	if err != nil || res.Outcome != PushDeferred {
		t.Fatalf("unreachable push: %+v, %v; want deferred", res, err)
	}
	if res.LocalHead != head {
		t.Fatalf("the local head is %s; want %s", res.LocalHead, head)
	}
	if got := gitIn(t, f.repo.dir, "rev-parse", "refs/heads/dev-int"); got != head {
		t.Fatalf("the integration branch moved during a deferred push: %s", got)
	}
}
