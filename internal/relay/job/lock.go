// lock.go serialises the lifecycle writes of one workspace's bg store (CRW-1155, CRW-1092). The oracle had no lock: every writer wrote
// the snapshot it had read, so a delivery stamp, an adoption, a reconciliation and a cancel could each put back what another had
// just written, and two hooks could select the same completion. Here a writer takes an exclusive flock on the bg directory itself,
// re-reads the record it is about to change and changes only its own fields. The directory is locked, not a lock file, so the
// store's tree holds no new entry. The lock is cooperative and covers only the writers of this package.

package job

import (
	"errors"
	"os"
	"syscall"
	"time"
)

// ErrStoreBusy is a lock that another writer kept for longer than LockWait.
const ErrStoreBusy = sentinel("the bg store is locked by another writer")

// lockWait bounds the wait for the store lock; a hook that cannot take it in time emits nothing and leaves its completions pending.
var lockWait = 5 * time.Second

// lockStore takes the store lock of the workspace and returns its release. A store that does not exist has nothing to lock: the error
// is then os.ErrNotExist. The handle is opened close-on-exec, so a job started while it is held never carries it.
func lockStore(ws string) (func(), error) {
	dir := BGDir(ws)
	if err := inside(ws, dir); err != nil {
		return nil, err
	}
	f, err := os.Open(dir)
	if err != nil {
		return nil, err
	}
	deadline := time.Now().Add(lockWait)
	for {
		err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return func() { _ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN); _ = f.Close() }, nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EINTR) {
			_ = f.Close()
			return nil, err
		}
		if time.Now().After(deadline) {
			_ = f.Close()
			return nil, ErrStoreBusy
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// withLock runs fn under the store lock unless the caller already holds it.
func withLock(ws string, held bool, fn func() error) error {
	if held {
		return fn()
	}
	unlock, err := lockStore(ws)
	if err != nil {
		return err
	}
	defer unlock()
	return fn()
}

// change is what an update decides about the record as it is on disk now: the record to write (write false leaves the file alone) and
// the ledger row that follows the write (nil for none).
type change struct {
	next  BgRecord
	event Event
	write bool
}

// update re-reads the record of id under the store lock and applies decide to it, so a writer never puts back a snapshot that another
// writer has changed since it was read. It returns the record as it is on disk afterwards; a record that is gone or does not read is
// errRecordGone, and nothing is written.
func update(ws, id string, clock func() time.Time, held bool, decide func(cur BgRecord) change) (BgRecord, error) {
	var out BgRecord
	err := withLock(ws, held, func() error {
		cur, err := readRecord(ws, id)
		if err != nil {
			return err
		}
		c := decide(cur)
		if !c.write {
			out = cur
			return nil
		}
		if err := writeRecord(ws, c.next, clock); err != nil {
			out = cur
			return err
		}
		if c.event != nil {
			_ = appendLedger(ws, c.event, clock)
		}
		out = c.next
		return nil
	})
	return out, err
}
