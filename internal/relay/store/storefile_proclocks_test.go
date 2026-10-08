//go:build linux

package store

import (
	"errors"
	"fmt"
	"testing"
)

// The reader tests feed storeFileLocks a recorded sequence of /proc/locks texts through
// readProcLocks, so a read that misses a held line is reproduced without a live kernel (CRW-1054).

// scriptedProcLocks returns a reader that serves the texts in order and then keeps serving the last
// one, counting its calls.
type scriptedProcLocks struct {
	texts []string
	err   error
	calls int
}

func (s *scriptedProcLocks) read() (string, error) {
	s.calls++
	if s.err != nil {
		return "", s.err
	}
	i := s.calls - 1
	if i >= len(s.texts) {
		i = len(s.texts) - 1
	}
	return s.texts[i], nil
}

// swapProcLocks installs a scripted reader for one test and restores the real one afterwards.
func swapProcLocks(t *testing.T, s *scriptedProcLocks) {
	t.Helper()
	saved := readProcLocks
	readProcLocks = s.read
	t.Cleanup(func() { readProcLocks = saved })
}

const (
	crw1054Pid   = 3095239
	crw1054Inode = 106078217
)

// crw1054HeldLine is this process's POSIX lock on the store's main inode, as /proc/locks prints it.
var crw1054HeldLine = fmt.Sprintf("11: POSIX  ADVISORY  READ %d 103:07:%d 1073741826 1073742335", crw1054Pid, crw1054Inode)

// crw1054OtherLine is an unrelated lock, so a text with only this line holds no lock of ours.
const crw1054OtherLine = "12: POSIX  ADVISORY  WRITE 4000000 103:07:999999999 0 EOF"

// TestStoreFileLocksFindsAHeldLockThatOneReadMisses is the CRW-1054 case: the first read of
// /proc/locks does not show the held lock (a seq_file read skipped it while other processes changed
// the lock list), and the next read does. The count must be 1, not the 0 of the first read.
func TestStoreFileLocksFindsAHeldLockThatOneReadMisses(t *testing.T) {
	s := &scriptedProcLocks{texts: []string{
		crw1054OtherLine + "\n",
		crw1054OtherLine + "\n" + crw1054HeldLine + "\n",
	}}
	swapProcLocks(t, s)
	count, uncounted := storeFileLocks(crw1054Pid, crw1054Inode)
	if count != 1 {
		t.Fatalf("storeFileLocks counted %d locks after a read that missed the held line and a read that showed it, want 1 (reads=%d, uncounted=%q)", count, s.calls, uncounted)
	}
}

// TestStoreFileLocksReadsOnceWhenTheFirstReadCounts pins the common path: a read that shows the lock
// returns at once, with no second read and no pause.
func TestStoreFileLocksReadsOnceWhenTheFirstReadCounts(t *testing.T) {
	s := &scriptedProcLocks{texts: []string{crw1054HeldLine + "\n"}}
	swapProcLocks(t, s)
	count, _ := storeFileLocks(crw1054Pid, crw1054Inode)
	if count != 1 || s.calls != 1 {
		t.Fatalf("storeFileLocks = %d after %d reads, want 1 after 1 read", count, s.calls)
	}
}

// TestStoreFileLocksReportsALostLock keeps the rule that a really lost lock fails: when every read
// misses the held line, the count is 0 and the bounded number of reads is spent, not more.
func TestStoreFileLocksReportsALostLock(t *testing.T) {
	s := &scriptedProcLocks{texts: []string{crw1054OtherLine + "\n"}}
	swapProcLocks(t, s)
	count, uncounted := storeFileLocks(crw1054Pid, crw1054Inode)
	if count != 0 {
		t.Fatalf("storeFileLocks counted %d locks when no read showed the held line, want 0", count)
	}
	if s.calls < 2 || s.calls > 10 {
		t.Fatalf("storeFileLocks read /proc/locks %d times for a lost lock, want a bounded retry (2 to 10)", s.calls)
	}
	if len(uncounted) != 0 {
		t.Fatalf("storeFileLocks reported uncounted lines %q for a text with no line naming the inode", uncounted)
	}
}

// TestStoreFileLocksKeepsALookupErrorAsMinusOne keeps a read failure a lookup error, not a count of
// zero, so judgeStoreFileLockAfter still refuses to call it a transient drop.
func TestStoreFileLocksKeepsALookupErrorAsMinusOne(t *testing.T) {
	s := &scriptedProcLocks{err: errors.New("procfs gone")}
	swapProcLocks(t, s)
	count, uncounted := storeFileLocks(crw1054Pid, crw1054Inode)
	if count != -1 || len(uncounted) != 1 {
		t.Fatalf("storeFileLocks = %d, %q on a read error, want -1 with one line", count, uncounted)
	}
}
