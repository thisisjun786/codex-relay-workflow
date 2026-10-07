package policystore

import (
	"os"
	"path/filepath"
	"testing"
)

// TestReadRefusesANamedPipe is the review finding: the policy path can be replaced with a named
// pipe after registration, and opening it would block the request for ever. The reader opens it
// without blocking and refuses anything that is not a regular file.
func TestReadRefusesANamedPipe(t *testing.T) {
	root := t.TempDir()
	fifo := filepath.Join(root, "policy.json")
	if err := mkfifo(fifo); err != nil {
		t.Skipf("this host cannot make a named pipe: %v", err)
	}
	done := make(chan error, 1)
	go func() { _, err := ReadRaw(fifo); done <- err }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("a named pipe was read as a policy")
		}
	case <-timeoutAfter(t):
		t.Fatal("reading a named pipe blocked")
	}
	// The same guard refuses it on the reading half.
	located := Located{State: Registered, Path: fifo}
	if reading := Read(located); reading.State != Unreadable {
		t.Fatalf("state = %q, want %q", reading.State, Unreadable)
	}
}

// TestReadRawRefusesADirectory is the same guard for any other non-regular path.
func TestReadRawRefusesADirectory(t *testing.T) {
	dir := t.TempDir()
	if _, err := ReadRaw(dir); err == nil {
		t.Fatal("a directory was read as a policy")
	}
	if _, err := ReadRaw(filepath.Join(dir, "absent.json")); err == nil {
		t.Fatal("an absent file was read as a policy")
	}
	if err := os.WriteFile(filepath.Join(dir, "ok.json"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadRaw(filepath.Join(dir, "ok.json")); err != nil {
		t.Fatalf("a regular file was refused: %v", err)
	}
}
