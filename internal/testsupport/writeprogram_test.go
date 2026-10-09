package testsupport_test

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport"
)

// CRW-1008: WriteProgram, the write of a file a test then executes, holds syscall.ForkLock for reading across the open and the close, as CopyBinary does. The lock is held for writing by every fork,
// so a write that has the read lock cannot overlap one.
func TestWriteProgramWaitsForAForkInProgressAndWritesTheFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "prog.sh")
	syscall.ForkLock.Lock() // a fork in progress
	done := make(chan error, 1)
	go func() { done <- testsupport.WriteProgram(path, []byte("#!/bin/sh\nexit 0\n"), 0o755) }()
	select {
	case err := <-done:
		syscall.ForkLock.Unlock()
		t.Fatalf("WriteProgram finished (%v) while a fork held the lock", err)
	case <-time.After(150 * time.Millisecond):
	}
	if _, err := os.Stat(path); err == nil {
		syscall.ForkLock.Unlock()
		t.Fatal("the file was opened while a fork held the lock")
	}
	syscall.ForkLock.Unlock()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("WriteProgram did not finish after the fork released the lock")
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0o755 {
		t.Fatalf("%v %v", info, err)
	}
	// it replaces what is there, as os.WriteFile does
	if err := testsupport.WriteProgram(path, []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if raw, _ := os.ReadFile(path); string(raw) != "#!/bin/sh\nexit 1\n" {
		t.Fatalf("content %q", raw)
	}
}
