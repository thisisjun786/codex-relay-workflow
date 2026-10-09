package cli

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// receiptLockRefusalWant is what a run answers when its lock file is not a regular file it can own: a refusal naming the lock file.
func receiptLockRefusalWant(t *testing.T, root string, got ReceiptCLIResult) {
	t.Helper()
	lockPath := expectedReceiptPath(root) + ".lock"
	if got.Code != 1 || !strings.Contains(got.Output, lockPath) || !strings.Contains(got.Output, "not a regular file") {
		t.Fatalf("run = %#v, want a refusal naming %s as not a regular file", got, lockPath)
	}
}

// A symlink at the lock file's path is refused, never followed (CRW-1013 evaluation d1, CRW-636). Followed to the receipt, the
// lock would sit on the receipt's own inode, which every publication replaces: the next run would lock another inode through
// the same name and its publication could land inside a withdrawal's compare-and-unlink window.
func TestReceiptLockFileSymlinkIsRefusedNotFollowed(t *testing.T) {
	for _, tc := range []struct {
		name   string
		target func(dir string) string
	}{
		{"to the receipt", func(string) string { return "test-receipt.json" }},
		{"dangling beside the receipt", func(string) string { return "nowhere" }},
		{"to a file elsewhere", func(dir string) string {
			elsewhere := filepath.Join(filepath.Dir(dir), "elsewhere")
			receiptMust(t, os.WriteFile(elsewhere, []byte("keep"), 0o644))
			return elsewhere
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := receiptRepo(t)
			t.Setenv("CRW_HOME", t.TempDir())
			dir := filepath.Dir(expectedReceiptPath(root))
			receiptMust(t, os.MkdirAll(dir, 0o777))
			target := tc.target(dir)
			link := expectedReceiptPath(root) + ".lock"
			receiptMust(t, os.Symlink(target, link))
			a := ReceiptCLIArgs{Verb: "test", Cwd: root, Session: "s1", Command: receiptCommand(t, "exit", "0")}
			got, err := RunReceiptCLI(a, ReceiptRunOptions{Stdout: io.Discard, Stderr: io.Discard})
			receiptMust(t, err)
			receiptLockRefusalWant(t, root, got)
			receiptAbsent(t, root)
			if info, err := os.Lstat(link); err != nil || info.Mode()&fs.ModeSymlink == 0 {
				t.Fatalf("the symlink was replaced or removed: %v %v", info, err)
			}
			if !filepath.IsAbs(target) {
				target = filepath.Join(dir, target)
			}
			if data, err := os.ReadFile(target); err == nil && len(data) != 0 && string(data) != "keep" {
				t.Fatalf("the run wrote through the symlink: %q", data)
			}
			if _, err := os.Stat(filepath.Join(dir, "nowhere")); !errors.Is(err, fs.ErrNotExist) {
				t.Fatalf("the run created the symlink's target: %v", err)
			}
		})
	}
}

// A FIFO at the lock file's path ends the run at once: opening it for reading would block with no writer, before the
// context-aware lock wait starts (CRW-1013 evaluation d2). The run refuses it as it refuses any lock file that is not
// regular, whether or not its context has ended.
func TestReceiptLockFileFifoIsRefusedWithoutBlocking(t *testing.T) {
	for _, tc := range []struct {
		name   string
		cancel bool
	}{{"live context", false}, {"cancelled context", true}} {
		t.Run(tc.name, func(t *testing.T) {
			root := receiptRepo(t)
			t.Setenv("CRW_HOME", t.TempDir())
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			fifo := expectedReceiptPath(root) + ".lock"
			receiptBeforePublishHook = func() { // the directory exists here
				receiptMust(t, syscall.Mkfifo(fifo, 0o600))
				if tc.cancel {
					cancel()
				}
			}
			defer func() { receiptBeforePublishHook = nil }()
			a := ReceiptCLIArgs{Verb: "test", Cwd: root, Session: "s1", Command: receiptCommand(t, "exit", "0")}
			done := make(chan ReceiptCLIResult, 1)
			failed := make(chan error, 1)
			go func() {
				got, err := RunReceiptCLI(a, ReceiptRunOptions{Context: ctx, Stdout: io.Discard, Stderr: io.Discard})
				done <- got
				failed <- err
			}()
			select {
			case got := <-done:
				receiptMust(t, <-failed)
				receiptLockRefusalWant(t, root, got)
			case <-time.After(10 * time.Second):
				// frees a run still blocked in the FIFO open so it ends with the test
				if w, err := os.OpenFile(fifo, os.O_WRONLY|syscall.O_NONBLOCK, 0); err == nil {
					_ = w.Close()
				}
				t.Fatal("a FIFO at the lock file's path blocks the run")
			}
			receiptAbsent(t, root)
		})
	}
}
