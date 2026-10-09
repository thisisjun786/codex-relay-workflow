package state

import (
	"os"
	"syscall"
	"testing"
	"time"
)

// CRW-1074 (post-evaluation d1): a session file that is not a regular file is unreadable and is never read, so a
// FIFO whose writer stays open cannot hold the caller.
func readStateWithin(t *testing.T, cwd string) bool {
	t.Helper()
	done := make(chan bool, 1)
	go func() { _, unreadable := ReadStateStrict(cwd, "s"); done <- unreadable }()
	select {
	case unreadable := <-done:
		return unreadable
	case <-time.After(5 * time.Second):
		t.Fatal("ReadStateStrict is blocked on a FIFO")
		return false
	}
}

func TestReadStateStrictDoesNotBlockOnAnOpenFIFO(t *testing.T) {
	cwd := t.TempDir()
	if err := makeSessionsDir(cwd); err != nil {
		t.Fatal(err)
	}
	path := StatePath(cwd, "s")
	if err := syscall.Mkfifo(path, 0o600); err != nil {
		t.Fatal(err)
	}
	holder, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Close()
	if !readStateWithin(t, cwd) {
		t.Fatal("a FIFO session file read as a clean state")
	}
}

func TestReadStateStrictRefusesAFIFOWithoutAWriter(t *testing.T) {
	cwd := t.TempDir()
	if err := makeSessionsDir(cwd); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(StatePath(cwd, "s"), 0o600); err != nil {
		t.Fatal(err)
	}
	if !readStateWithin(t, cwd) {
		t.Fatal("a FIFO session file read as a clean state")
	}
}
