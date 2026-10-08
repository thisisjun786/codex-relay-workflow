//go:build linux

package store

import (
	"testing"
)

// judgeStoreFileLockAfter applies the lock rule the two store-file lock tests share (CRW-888) to the
// count read after one step: a lookup error (-1) fails, only a count of 0 is a lost lock, and a
// lower non-zero count is logged, because the /proc/locks line count on one inode can carry a
// transient extra line. The count is read from the test's own process.
func judgeStoreFileLockAfter(t *testing.T, pid int, inode uint64, before int, where string) {
	t.Helper()
	after, uncounted := storeFileLocks(pid, inode)
	switch {
	case after < 0:
		t.Fatalf("the lock count on the store's main inode could not be read after %s (storeFileLocks returned %d, %v): a lookup error is not a transient drop (CRW-888)", where, after, uncounted)
	case after == 0:
		t.Fatalf("the store's POSIX lock was lost after %s: the process holds no lock on the store's main inode (%d lock(s) before, %d after)", where, before, after)
	case after < before:
		t.Logf("the store's POSIX lock count on the main inode fell from %d to %d after %s; a non-zero count is not a lost lock", before, after, where)
	}
}
