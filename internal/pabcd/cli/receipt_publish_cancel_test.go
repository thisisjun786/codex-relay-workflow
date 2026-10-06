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
// the bytes are this run's, the unlink is refused, and the receipt is still there.
func TestReceiptPublishCancellationWithdrawalFailure(t *testing.T) {
	root := receiptRepo(t)
	t.Setenv("CRW_HOME", t.TempDir())
	dir := filepath.Dir(expectedReceiptPath(root))
	probeDir := t.TempDir()
	receiptMust(t, os.Chmod(probeDir, 0o555))
	if probe, perr := os.Create(filepath.Join(probeDir, "probe")); perr == nil {
		_ = probe.Close()
		t.Skip("this process can write to a read-only directory (running as root?), so the unlink cannot be made to fail")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	receiptAfterPublishHook = func() {
		// The receipt is still this run's, but the directory refuses its unlink.
		receiptMust(t, os.Chmod(dir, 0o555))
		cancel()
	}
	defer func() { receiptAfterPublishHook = nil }()
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })
	a := ReceiptCLIArgs{Verb: "test", Cwd: root, Session: "s1", Command: receiptCommand(t, "exit", "0")}
	got, err := RunReceiptCLI(a, ReceiptRunOptions{Context: ctx, Stdout: io.Discard, Stderr: io.Discard})
	var pathErr *os.PathError
	if !errors.As(err, &pathErr) || pathErr.Op != "unlink" || pathErr.Path != expectedReceiptPath(root) {
		t.Fatalf("withdrawal failure = %v, want an unlink PathError for the receipt", err)
	}
	if got != (ReceiptCLIResult{}) {
		t.Fatalf("a failed withdrawal returned %#v, want the zero result", got)
	}
	if _, statErr := os.Stat(expectedReceiptPath(root)); statErr != nil {
		t.Fatalf("the receipt is gone although its withdrawal failed: %v", statErr)
	}
}

// A receipt another invocation published at the same fixed path in the meantime belongs to that
// invocation: the withdrawal leaves it alone and this run still answers the refusal.
func TestReceiptPublishCancellationLeavesAnotherInvocationsReceipt(t *testing.T) {
	root := receiptRepo(t)
	t.Setenv("CRW_HOME", t.TempDir())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	other := []byte("{\"kind\":\"test\",\"note\":\"another invocation\"}\n")
	receiptAfterPublishHook = func() {
		// Another invocation for the same session replaced the receipt at the same path.
		receiptMust(t, os.WriteFile(expectedReceiptPath(root), other, 0o644))
		cancel()
	}
	defer func() { receiptAfterPublishHook = nil }()
	a := ReceiptCLIArgs{Verb: "test", Cwd: root, Session: "s1", Command: receiptCommand(t, "exit", "0")}
	got := receiptRun(t, a, ReceiptRunOptions{Context: ctx})
	if got.Code != 1 || got.Output != receiptInterrupted {
		t.Fatalf("publish cancellation: %#v", got)
	}
	if data, err := os.ReadFile(expectedReceiptPath(root)); err != nil || string(data) != string(other) {
		t.Fatalf("the other invocation's receipt = %q, %v; this run's withdrawal must leave it", data, err)
	}
}

// The publish error mapping: a clean cancellation - the context's own error, returned after the
// temp file it had written was removed - is the refusal; a cancellation joined with a failed
// removal is reported as it happened, so a stranded temporary file is never hidden behind
// "no receipt written".
func TestReceiptPublishCancellationWithAStrandedTempFileIsReported(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	joined := errors.Join(context.Canceled, errors.New("unlink .test-receipt.json.x.tmp: permission denied"))
	if got, refused := receiptPublishRefusal(ctx, joined); refused || got != (ReceiptCLIResult{}) {
		t.Fatalf("a joined cancellation = %#v, %v; want it reported instead of the refusal", got, refused)
	}
	if got, refused := receiptPublishRefusal(ctx, context.Canceled); !refused || got != (ReceiptCLIResult{Output: receiptInterrupted, Code: 1}) {
		t.Fatalf("a clean cancellation = %#v, %v; want the interrupted refusal", got, refused)
	}
}
