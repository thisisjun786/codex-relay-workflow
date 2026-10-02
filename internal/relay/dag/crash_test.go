package dag

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"reflect"
	"syscall"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

const (
	killDB   = "CRW_DAG_KILL_DB"
	killStep = "CRW_DAG_KILL_STEP"
)

// TestKilledWriteHelper is the child process of the kill tests below and is no test of its own: it runs
// only when the parent names a store. It opens that store, starts a revision, and kills its own process
// (SIGKILL: no deferred function runs, no rollback, no close) at the one point the store's transaction
// contract offers - the revision fully written and not yet committed.
func TestKilledWriteHelper(t *testing.T) {
	path := os.Getenv(killDB)
	if path == "" {
		t.Skip("the child process of the kill tests")
	}
	s, err := store.Open(context.Background(), path, "")
	if err != nil {
		t.Fatal(err)
	}
	s.SetFaultHook(func() { _ = syscall.Kill(syscall.Getpid(), syscall.SIGKILL) })
	revs := revisions()
	step := 0
	if os.Getenv(killStep) == "second" {
		step = 1
	}
	_, err = (&Repo{Store: s}).Put(context.Background(), decode(t, revs[step]))
	t.Fatalf("the process outlived its own SIGKILL: %v", err)
}

// kill runs the helper against the store at path and requires that SIGKILL ended it.
func kill(t *testing.T, path, step string) {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestKilledWriteHelper$", "-test.count=1")
	cmd.Env = append(os.Environ(), killDB+"="+path, killStep+"="+step)
	out, err := cmd.CombinedOutput()
	var exit *exec.ExitError
	if !errors.As(err, &exit) {
		t.Fatalf("the helper was not killed: %v\n%s", err, out)
	}
	if status, ok := exit.Sys().(syscall.WaitStatus); !ok || !status.Signaled() || status.Signal() != syscall.SIGKILL {
		t.Fatalf("the helper ended by %v, not SIGKILL\n%s", exit, out)
	}
}

// A process killed while its first revision is written but not committed leaves no plan, no row of one, and a store
// that opens and takes the same request afterwards.
func TestKilledDuringTheFirstRevisionLeavesNoPlan(t *testing.T) {
	r, _, path := newRepo(t)
	kill(t, path, "first")

	fresh := &Repo{Store: openStore(t, path)}
	if rows := zoneRows(t, fresh.Store.DB); total(rows) != 0 {
		t.Fatalf("the killed write left rows: %v", rows)
	}
	if _, _, err := fresh.Snapshot(context.Background(), "plan", 0); reasonOf(err) != "unregistered_scope" {
		t.Fatalf("a plan whose only revision was never committed reads as %v", err)
	}
	// the request that died is new to the store: it is written, not replayed
	res, err := r.Put(context.Background(), decode(t, revisions()[0]))
	if err != nil || res.Replayed || res.RevisionNo != 1 {
		t.Fatalf("the retry: %+v %v", res, err)
	}
	if err := fresh.VerifyLog(context.Background(), "plan"); err != nil {
		t.Fatal(err)
	}
}

// A process killed while the second revision is written but not committed leaves the first as the last committed
// revision: reads return it, nothing of the second is in any table, and the second can be sent again.
func TestKilledDuringALaterRevisionKeepsTheLastCommittedOne(t *testing.T) {
	r, _, path := newRepo(t)
	first := mustPut(t, r, revisions()[0])
	committed := zoneRows(t, r.Store.DB)

	kill(t, path, "second")

	fresh := &Repo{Store: openStore(t, path)}
	ctx := context.Background()
	if after := zoneRows(t, fresh.Store.DB); !reflect.DeepEqual(after, committed) {
		t.Fatalf("the killed write left a trace:\n before %v\n after  %v", committed, after)
	}
	snap, head, err := fresh.Snapshot(ctx, "plan", 0)
	if err != nil || head != 1 || snap.Revision != 1 || snap.StateDigest != first.StateDigest {
		t.Fatalf("after the kill the plan reads as revision %d (head %d, %v), want the committed revision 1", snap.Revision, head, err)
	}
	for _, n := range snap.Nodes {
		if n.IntroducedRev != 1 {
			t.Fatalf("node %s of an uncommitted revision is in the plan", n.NodeID)
		}
	}
	page, err := fresh.Events(ctx, "plan", 0, 10)
	if err != nil || len(page.Events) != 1 || page.Head != 1 {
		t.Fatalf("the log after the kill: %+v %v", page, err)
	}
	if err := fresh.VerifyLog(ctx, "plan"); err != nil {
		t.Fatal(err)
	}
	res, err := fresh.Put(ctx, decode(t, revisions()[1]))
	if err != nil || res.Replayed || res.RevisionNo != 2 {
		t.Fatalf("the retry of the killed request: %+v %v", res, err)
	}
}
