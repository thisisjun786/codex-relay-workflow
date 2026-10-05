package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"
)

// A cancellation that lands while the receipt is being published, after the rename: the receipt
// just published is withdrawn and the same refusal the late-cancel check gives is returned. The
// seam (receiptAfterPublishHook) exists only to reach that window deterministically - the real one
// is a race. On the code before this change the same window published the receipt and returned its
// path with code 0.
func TestReceiptPublishCancellationWithdrawsTheReceipt(t *testing.T) {
	root := receiptRepo(t)
	t.Setenv("CRW_HOME", t.TempDir())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	atHook := errors.New("the after-publish hook did not run")
	receiptAfterPublishHook = func() {
		atHook = nil
		if _, err := os.Stat(expectedReceiptPath(root)); err != nil {
			atHook = fmt.Errorf("no completed receipt when the hook runs: %w", err)
		}
		cancel()
	}
	defer func() { receiptAfterPublishHook = nil }()
	a := ReceiptCLIArgs{Verb: "test", Cwd: root, Session: "s1", Command: receiptCommand(t, "exit", "0")}
	got := receiptRun(t, a, ReceiptRunOptions{Context: ctx})
	if atHook != nil {
		t.Fatalf("publish window: %v", atHook)
	}
	if got.Code != 1 || got.Output != receiptInterrupted {
		t.Fatalf("publish cancellation: %#v", got)
	}
	receiptAbsent(t, root)
}

// A cancellation that lands between the late-cancel check and the rename is reported by
// PublishContext and maps to the same refusal: nothing is published and the after-publish hook
// must not run.
func TestReceiptPublishCancellationReturnsInterruptedRefusal(t *testing.T) {
	root := receiptRepo(t)
	t.Setenv("CRW_HOME", t.TempDir())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	receiptBeforePublishHook = cancel
	defer func() { receiptBeforePublishHook = nil }()
	afterRan := false
	receiptAfterPublishHook = func() { afterRan = true }
	defer func() { receiptAfterPublishHook = nil }()
	a := ReceiptCLIArgs{Verb: "test", Cwd: root, Session: "s1", Command: receiptCommand(t, "exit", "0")}
	got := receiptRun(t, a, ReceiptRunOptions{Context: ctx})
	if afterRan {
		t.Fatalf("the after-publish hook ran on a refused publish")
	}
	if got.Code != 1 || got.Output != receiptInterrupted {
		t.Fatalf("mid-publish cancellation: %#v", got)
	}
	receiptAbsent(t, root)
	if left, err := filepath.Glob(filepath.Join(filepath.Dir(expectedReceiptPath(root)), ".*.tmp")); err != nil || len(left) != 0 {
		t.Fatalf("temp files %v, %v", left, err)
	}
}

// A withdrawal that fails reports the unlink failure instead of claiming no receipt was written:
// the receipt is still there.
func TestReceiptPublishCancellationWithdrawalFailure(t *testing.T) {
	root := receiptRepo(t)
	t.Setenv("CRW_HOME", t.TempDir())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	receiptAfterPublishHook = func() {
		// Replace the published receipt with something unlink(2) refuses: a non-empty directory.
		path := expectedReceiptPath(root)
		receiptMust(t, os.Remove(path))
		receiptMust(t, os.MkdirAll(filepath.Join(path, "blocker"), 0o755))
		cancel()
	}
	defer func() { receiptAfterPublishHook = nil }()
	a := ReceiptCLIArgs{Verb: "test", Cwd: root, Session: "s1", Command: receiptCommand(t, "exit", "0")}
	got, err := RunReceiptCLI(a, ReceiptRunOptions{Context: ctx, Stdout: io.Discard, Stderr: io.Discard})
	var pathErr *os.PathError
	if !errors.As(err, &pathErr) || pathErr.Op != "unlink" || pathErr.Path != expectedReceiptPath(root) {
		t.Fatalf("withdrawal failure = %v, want an unlink PathError for the receipt", err)
	}
	if got != (ReceiptCLIResult{}) {
		t.Fatalf("a failed withdrawal returned %#v, want the zero result", got)
	}
	if _, statErr := os.Stat(filepath.Join(expectedReceiptPath(root), "blocker")); statErr != nil {
		t.Fatalf("the refused path changed: %v", statErr)
	}
}
