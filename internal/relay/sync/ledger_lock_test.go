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
)

// lockRow is one flock line of /proc/locks.
type lockRow struct {
	blocked bool   // a request waiting behind another lock, listed with "->" after the number
	pid     string // the process that holds or requests the lock
	file    string // "major:minor:inode": the device in hex, the inode in decimal
}

// flockRows lists the flock lines of /proc/locks. The holder of a lock and a request blocked behind it
// read alike, except for the arrow:
//
//	68: FLOCK  ADVISORY  WRITE 3108925 103:07:31462887 0 EOF
//	68: -> FLOCK  ADVISORY  WRITE 3108925 103:07:31462887 0 EOF
func flockRows() ([]lockRow, error) {
	raw, err := os.ReadFile("/proc/locks")
	if err != nil {
		return nil, err
	}
	var rows []lockRow
	for _, line := range strings.Split(string(raw), "\n") {
		fields := strings.Fields(line)
		blocked := len(fields) > 1 && fields[1] == "->"
		if blocked {
			fields = append(fields[:1], fields[2:]...)
		}
		if len(fields) >= 6 && fields[1] == "FLOCK" {
			rows = append(rows, lockRow{blocked: blocked, pid: fields[4], file: fields[5]})
		}
	}
	return rows, nil
}

// holderFile returns how /proc/locks names the sidecar this process holds a flock on. The inode and the
// process pick the holder's line out of the system's locks, and the name it prints, device included, is
// what the waiter's line must match, so a request blocked on another file never counts.
func holderFile(sidecar os.FileInfo) (string, error) {
	stat, ok := sidecar.Sys().(*syscall.Stat_t)
	if !ok {
		return "", fmt.Errorf("no inode in %T", sidecar.Sys())
	}
	rows, err := flockRows()
	if err != nil {
		return "", err
	}
	pid, suffix := strconv.Itoa(os.Getpid()), ":"+strconv.FormatUint(stat.Ino, 10)
	var files []string
	for _, row := range rows {
		if !row.blocked && row.pid == pid && strings.HasSuffix(row.file, suffix) {
			files = append(files, row.file)
		}
	}
	if len(files) != 1 {
		return "", fmt.Errorf("/proc/locks lists %d flocks held by pid %s on inode %d, want the holder's one", len(files), pid, stat.Ino)
	}
	return files[0], nil
}

// waitForWaiterToBlock returns once the waiter is seen blocked behind the holder's lock on the sidecar,
// the only moment the holder may let go: a waiter that returns first did not wait, and one that is never
// seen blocked is not shown to have waited. Without /proc/locks (not Linux) it can only check that the
// waiter has not returned after a short while, which misses a lock-free waiter that first runs later
// than that but never fails one that waits.
func waitForWaiterToBlock(t *testing.T, sidecar os.FileInfo, done <-chan error) {
	t.Helper()
	returned := func(e error) {
		t.Helper()
		t.Fatalf("WithLedgerLock returned (%v) while another holder still held the sidecar lock", e)
	}
	if runtime.GOOS != "linux" {
		t.Logf("no /proc/locks here: checking only that the waiter has not returned after %v", lockWaitUnfinishedFor)
		select {
		case e := <-done:
			returned(e)
		case <-time.After(lockWaitUnfinishedFor):
		}
		return
	}
	file, err := holderFile(sidecar)
	if err != nil {
		t.Fatalf("cannot observe the waiter: %v", err)
	}
	pid := strconv.Itoa(os.Getpid())
	deadline := time.NewTimer(lockWaitBound)
	defer deadline.Stop()
	tick := time.NewTicker(5 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case e := <-done:
			returned(e)
		case <-deadline.C:
			t.Fatalf("no request blocked behind the sidecar lock (%s) was listed in /proc/locks within %v: WithLedgerLock did not wait for its holder", file, lockWaitBound)
		case <-tick.C:
			rows, err := flockRows()
			if err != nil {
				t.Fatalf("cannot observe the waiter: %v", err)
			}
			for _, row := range rows {
				if row.blocked && row.pid == pid && row.file == file {
					return
				}
			}
		}
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
