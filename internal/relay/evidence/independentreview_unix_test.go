//go:build unix

package evidence

import (
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// A file swapped for a FIFO between the parent choosing to read it and the open would otherwise
// block the whole merge-evidence run until someone writes to it.
func TestOpenRegularDoesNotWaitOnAFIFO(t *testing.T) {
	path := filepath.Join(t.TempDir(), "swapped.json")
	if err := syscall.Mkfifo(path, 0o600); err != nil {
		t.Skip("no FIFO here:", err)
	}
	done := make(chan error, 1)
	go func() { _, err := readArtifactFile(path); done <- err }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("a FIFO was read as an artifact")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the open of a FIFO is still waiting after five seconds")
	}
}
