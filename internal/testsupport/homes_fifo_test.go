//go:build unix

package testsupport

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// A link to a FIFO is recorded with the target's mode and never opened: opening one blocks until a writer appears, so a
// snapshot that read it would hang (CRW-1182 verification). A link to a device is metadata too (reading /dev/zero is
// unbounded), and a regular file behind a link is still read.
func TestContentsRecordsALinkToAFIFOWithoutOpeningIt(t *testing.T) {
	h, codex := linkHomes(t)
	fifo := filepath.Join(t.TempDir(), "pipe")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Skipf("no FIFO here: %v", err)
	}
	if err := os.Symlink(fifo, filepath.Join(codex, "pipe-link")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat("/dev/zero"); err == nil {
		if err := os.Symlink("/dev/zero", filepath.Join(codex, "zero-link")); err != nil {
			t.Fatal(err)
		}
	}
	type result struct {
		out string
		err error
	}
	done := make(chan result, 1)
	go func() {
		out, err := h.Contents()
		done <- result{out, err}
	}()
	select {
	case r := <-done:
		if r.err != nil {
			t.Fatalf("Contents: %v", r.err)
		}
		if !strings.Contains(r.out, "pipe-link Lrwxrwxrwx -> "+fifo+" (target prw") {
			t.Fatalf("the FIFO link is not recorded as metadata:\n%s", r.out)
		}
		if strings.Contains(r.out, "sha256:") && strings.Contains(r.out, "pipe-link Lrwxrwxrwx -> "+fifo+" sha256") {
			t.Fatalf("the FIFO link was read:\n%s", r.out)
		}
		if _, err := os.Stat("/dev/zero"); err == nil && !strings.Contains(r.out, "zero-link Lrwxrwxrwx -> /dev/zero (target ") {
			t.Fatalf("the device link is not recorded as metadata:\n%s", r.out)
		}
	case <-time.After(5 * time.Second):
		// Release the blocked open so the goroutine ends, then fail.
		if w, err := os.OpenFile(fifo, os.O_WRONLY|syscall.O_NONBLOCK, 0); err == nil {
			_ = w.Close()
		}
		t.Fatal("Contents blocked on a link to a FIFO")
	}
}
