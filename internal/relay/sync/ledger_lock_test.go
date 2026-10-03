package sync

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/evidence"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/reception"
	"golang.org/x/sys/unix"
)

const (
	// lockWaitBound bounds how long the test looks for the waiter blocked behind the holder, and how long
	// the cleanup gives a waiter that was still running when the test ended.
	lockWaitBound = 10 * time.Second
	// lockWaitUnfinishedFor is how long the waiter must stay unfinished where there is no /proc/locks.
	lockWaitUnfinishedFor = 500 * time.Millisecond
	// lockPollEvery is how often the test looks again for the waiter in /proc/locks.
	lockPollEvery = 5 * time.Millisecond
)

// lockRow is one flock line of /proc/locks.
type lockRow struct {
	blocked bool   // a request waiting behind another lock, listed with "->" after the number
	pid     string // the process that holds or requests the lock
	file    string // "major:minor:inode": the device in hex, the inode in decimal
}

// parseFlockRows lists the flock lines of one /proc/locks snapshot. The holder of a lock and a request
// blocked behind it read alike, except for the arrow:
//
//	68: FLOCK  ADVISORY  WRITE 3108925 103:07:31462887 0 EOF
//	68: -> FLOCK  ADVISORY  WRITE 3108925 103:07:31462887 0 EOF
func parseFlockRows(raw string) []lockRow {
	var rows []lockRow
	for _, line := range strings.Split(raw, "\n") {
		fields := strings.Fields(line)
		blocked := len(fields) > 1 && fields[1] == "->"
		if blocked {
			fields = append(fields[:1], fields[2:]...)
		}
		if len(fields) >= 6 && fields[1] == "FLOCK" {
			rows = append(rows, lockRow{blocked: blocked, pid: fields[4], file: fields[5]})
		}
	}
	return rows
}

// flockRows reads one snapshot of the system's flocks.
func flockRows() ([]lockRow, error) {
	raw, err := os.ReadFile("/proc/locks")
	if err != nil {
		return nil, err
	}
	return parseFlockRows(string(raw)), nil
}

// sidecarView is what one snapshot shows about the flock this process holds on the sidecar.
type sidecarView struct {
	held    []string  // the files with the sidecar's inode number that this process holds a flock on, as /proc/locks names them, one per row
	blocked bool      // a request of this process is blocked behind the lock it holds on the only such file
	own     []lockRow // every flock row of this process in the snapshot, for failure messages
}

// observeSidecar reads what rows show about the lock pid holds on the file with inode number ino. The inode
// and the process pick the holder's line out of the system's locks, and the name it prints, device included,
// is what the waiter's line must match, so a request blocked on another file never counts.
func observeSidecar(rows []lockRow, pid string, ino uint64) sidecarView {
	suffix := ":" + strconv.FormatUint(ino, 10)
	var view sidecarView
	for _, row := range rows {
		if row.pid != pid {
			continue
		}
		view.own = append(view.own, row)
		if !row.blocked && strings.HasSuffix(row.file, suffix) {
			view.held = append(view.held, row.file)
		}
	}
	if len(view.held) == 1 {
		for _, row := range view.own {
			if row.blocked && row.file == view.held[0] {
				view.blocked = true
				break
			}
		}
	}
	return view
}

// errWaiterReturned is the failure of a waiter that finished while the holder still held the sidecar lock.
func errWaiterReturned(e error) error {
	return fmt.Errorf("WithLedgerLock returned (%v) while another holder still held the sidecar lock", e)
}

// awaitBlockedWaiter returns nil once a snapshot shows the waiter blocked behind the holder's lock on the file
// with inode number ino, and an error when the waiter returns first, when snapshot fails, or when bound passes
// without that sight. The first snapshot must list the holder.
func awaitBlockedWaiter(snapshot func() ([]lockRow, error), pid string, ino uint64, bound, tick time.Duration, done <-chan error) error {
	rows, err := snapshot()
	if err != nil {
		return fmt.Errorf("cannot observe the waiter: %w", err)
	}
	view := observeSidecar(rows, pid, ino)
	if len(view.held) != 1 {
		return fmt.Errorf("cannot observe the waiter: /proc/locks lists %d flocks held by pid %s on inode %d, want the holder's one", len(view.held), pid, ino)
	}
	deadline := time.NewTimer(bound)
	defer deadline.Stop()
	poll := time.NewTicker(tick)
	defer poll.Stop()
	for {
		select {
		case e := <-done:
			return errWaiterReturned(e)
		case <-deadline.C:
			return fmt.Errorf("no request blocked behind the sidecar lock (%s) was listed in /proc/locks within %v: WithLedgerLock did not wait for its holder", view.held[0], bound)
		case <-poll.C:
			rows, err := snapshot()
			if err != nil {
				return fmt.Errorf("cannot observe the waiter: %w", err)
			}
			if observeSidecar(rows, pid, ino).blocked {
				return nil
			}
		}
	}
}

// waitForWaiterToBlock returns once the waiter is seen blocked behind the holder's lock on the sidecar,
// the only moment the holder may let go: a waiter that returns first did not wait, and one that is never
// seen blocked is not shown to have waited. Without /proc/locks (not Linux) it can only check that the
// waiter has not returned after a short while, which misses a lock-free waiter that first runs later
// than that but never fails one that waits.
func waitForWaiterToBlock(t *testing.T, sidecar os.FileInfo, done <-chan error) {
	t.Helper()
	if runtime.GOOS != "linux" {
		t.Logf("no /proc/locks here: checking only that the waiter has not returned after %v", lockWaitUnfinishedFor)
		select {
		case e := <-done:
			t.Fatal(errWaiterReturned(e))
		case <-time.After(lockWaitUnfinishedFor):
		}
		return
	}
	stat, ok := sidecar.Sys().(*syscall.Stat_t)
	if !ok {
		t.Fatalf("cannot observe the waiter: no inode in %T", sidecar.Sys())
	}
	if err := awaitBlockedWaiter(flockRows, strconv.Itoa(os.Getpid()), stat.Ino, lockWaitBound, lockPollEvery, done); err != nil {
		t.Fatal(err)
	}
}

// The Go half of what was a Python/Go ledger interop test. The Python half (a Python process
// holding the ledger lock while Go waits, Python writing the ledger Go reads and reading back
// what Go wrote) is gone: rollback to Python closed at todo 43 (rollback_allowed=0), and the
// Python runtime leaves in todo 44. What stays is Go's own contract: WithLedgerLock waits for
// another holder of the sidecar, completes its read/decision/write once that holder lets go, and
// never replaces the sidecar. The holder lets go only after the waiter is seen blocked behind it
// (waitForWaiterToBlock), so a WithLedgerLock that did not wait cannot pass.
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
	finished := make(chan struct{})
	go func() {
		defer close(finished)
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
	// The deferred holder.Close has released the lock by the time Cleanup runs, so a waiter still
	// blocked when the test failed runs on and ends before the temp directory is removed.
	t.Cleanup(func() {
		select {
		case <-finished:
		case <-time.After(lockWaitBound):
			t.Errorf("the waiter was still running %v after the holder closed the sidecar", lockWaitBound)
		}
	})
	waitForWaiterToBlock(t, before, done)
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
