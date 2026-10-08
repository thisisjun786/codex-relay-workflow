package configguard

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// CRW-993 generation 2, d6 second clause: the handover's FIFO writers are bounded, so a regression fails fast
// instead of hanging the package. A FIFO that no reader opens must make the writer's open return an error
// within the bound.
func TestConfigLockPathsFifoWriterIsBounded(t *testing.T) {
	prev := configLockPathsFifoDeadline
	configLockPathsFifoDeadline = 200 * time.Millisecond
	t.Cleanup(func() { configLockPathsFifoDeadline = prev })
	fifo := filepath.Join(t.TempDir(), "manifest")
	if err := unix.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	f, err := configLockPathsOpenFifoWriter(fifo)
	if err == nil {
		_ = f.Close()
		t.Fatal("a FIFO with no reader was opened for writing")
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("the open took %s, past its bound", elapsed)
	}
}

// configLockPathsFifoDeadline bounds the handover's wait for a reader on a FIFO (CRW-993 d6).
var configLockPathsFifoDeadline = 20 * time.Second

// configLockPathsOpenFifoWriter opens a FIFO for writing once a reader has it open, and fails after
// configLockPathsFifoDeadline instead of blocking the package.
func configLockPathsOpenFifoWriter(path string) (*os.File, error) {
	deadline := time.Now().Add(configLockPathsFifoDeadline)
	for {
		f, err := os.OpenFile(path, os.O_WRONLY|syscall.O_NONBLOCK, 0)
		if err == nil {
			if err := unix.SetNonblock(int(f.Fd()), false); err != nil {
				_ = f.Close()
				return nil, err
			}
			return f, nil
		}
		if !errors.Is(err, syscall.ENXIO) {
			return nil, err
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("no reader opened %s within %s", path, configLockPathsFifoDeadline)
		}
		time.Sleep(time.Millisecond)
	}
}
