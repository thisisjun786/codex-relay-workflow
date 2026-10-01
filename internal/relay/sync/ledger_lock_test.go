package sync

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/evidence"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/reception"
	"golang.org/x/sys/unix"
)

// The Go half of what was a Python/Go ledger interop test. The Python half (a Python process
// holding the ledger lock while Go waits, Python writing the ledger Go reads and reading back
// what Go wrote) is gone: rollback to Python closed at todo 43 (rollback_allowed=0), and the
// Python runtime leaves in todo 44. What stays is Go's own contract: WithLedgerLock waits for
// another holder of the sidecar, completes its read/decision/write once that holder lets go, and
// never replaces the sidecar.
func Test23_LedgerLockWaitsForItsHolderAndKeepsTheSidecar(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "ledger.json")
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if e := os.MkdirAll(filepath.Dir(path), 0o755); e != nil {
		t.Fatal(e)
	}
	if e := reception.SaveLedger(path, reception.EmptyLedger("child")); e != nil {
		t.Fatal(e)
	}
	holder, e := os.OpenFile(path+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if e != nil {
		t.Fatal(e)
	}
	defer holder.Close()
	if e = unix.Flock(int(holder.Fd()), unix.LOCK_EX); e != nil {
		t.Fatal(e)
	}
	probe, e := os.OpenFile(path+".lock", os.O_RDWR, 0o600)
	if e != nil {
		t.Fatal(e)
	}
	if e = unix.Flock(int(probe.Fd()), unix.LOCK_EX|unix.LOCK_NB); e != unix.EWOULDBLOCK {
		t.Fatalf("the holder's lock must exclude another open of the sidecar: %v", e)
	}
	if e = probe.Close(); e != nil {
		t.Fatal(e)
	}
	before, e := os.Stat(path + ".lock")
	if e != nil {
		t.Fatal(e)
	}
	done := make(chan error, 1)
	go func() {
		done <- reception.WithLedgerLock(path, func() error {
			ledger, e := reception.LoadLedger(path, "child")
			if e != nil {
				return e
			}
			answers, _ := evidence.Object(reception.Get(ledger, "answered"))
			reception.Set(&answers, "msg", reception.O("contentDigest", "digest", "disposition", "accepted", "applied", false, "toldToAct", true))
			reception.Set(&ledger, "answered", answers)
			return reception.SaveLedger(path, ledger)
		})
	}()
	if e = unix.Flock(int(holder.Fd()), unix.LOCK_UN); e != nil {
		t.Fatal(e)
	}
	select {
	case e := <-done:
		if e != nil {
			t.Fatal(e)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	read, e := reception.LoadLedger(path, "child")
	if e != nil {
		t.Fatal(e)
	}
	entry := reception.Get(reception.Get(read, "answered"), "msg")
	if reception.Get(entry, "contentDigest") != "digest" || reception.Get(entry, "toldToAct") != true || reception.Get(entry, "applied") != false {
		t.Fatal(pyjson.Dumps(read, pyjson.Options{}))
	}
	after, e := os.Stat(path + ".lock")
	if e != nil {
		t.Fatal(e)
	}
	if !os.SameFile(before, after) {
		t.Fatal("lock sidecar inode replaced")
	}
}
