package cli

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/crwdir"
)

// publishThenFailSync is the real publication followed by a failed directory sync after the rename, as PublishContext
// reports it: the receipt is in place and the error is a *PublishedError.
func publishThenFailSync(after func()) func(context.Context, string, []byte) error {
	return func(ctx context.Context, path string, data []byte) error {
		if err := crwdir.PublishContext(ctx, path, data); err != nil {
			return err
		}
		if after != nil {
			after()
		}
		return &crwdir.PublishedError{Err: syscall.EIO}
	}
}

// A cancellation that lands while the receipt is published, with a directory sync that then fails, withdraws the receipt
// before the sync error is returned: the cancelled run must not leave a passing receipt behind (CRW-1029 evaluation d1).
func TestReceiptCancelledPublishWithAFailedDirectorySyncWithdrawsTheReceipt(t *testing.T) {
	root := receiptRepo(t)
	t.Setenv("CRW_HOME", t.TempDir())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	receiptPublish = publishThenFailSync(cancel)
	defer func() { receiptPublish = crwdir.PublishContext }()
	a := ReceiptCLIArgs{Verb: "test", Cwd: root, Session: "s1", Command: receiptCommand(t, "exit", "0")}
	_, err := RunReceiptCLI(a, ReceiptRunOptions{Context: ctx, Stdout: io.Discard, Stderr: io.Discard})
	if !errors.Is(err, syscall.EIO) || !crwdir.Published(err) {
		t.Fatalf("error = %v, want the published sync error", err)
	}
	receiptAbsent(t, root)
}

// Without a cancellation the receipt of a published, unsynced publication stands (CRW-802) and the error is returned.
func TestReceiptPublishedSyncErrorWithoutCancellationKeepsTheReceipt(t *testing.T) {
	root := receiptRepo(t)
	t.Setenv("CRW_HOME", t.TempDir())
	receiptPublish = publishThenFailSync(nil)
	defer func() { receiptPublish = crwdir.PublishContext }()
	a := ReceiptCLIArgs{Verb: "test", Cwd: root, Session: "s1", Command: receiptCommand(t, "exit", "0")}
	_, err := RunReceiptCLI(a, ReceiptRunOptions{Context: context.Background(), Stdout: io.Discard, Stderr: io.Discard})
	if !errors.Is(err, syscall.EIO) || !crwdir.Published(err) {
		t.Fatalf("error = %v, want the published sync error", err)
	}
	if _, statErr := os.Stat(expectedReceiptPath(root)); statErr != nil {
		t.Fatalf("the published receipt is gone: %v", statErr)
	}
}

// A withdrawal that fails after the sync error keeps both errors.
func TestReceiptCancelledPublishWithAFailedSyncReportsAFailedWithdrawal(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root: the unlink cannot be made to fail")
	}
	root := receiptRepo(t)
	t.Setenv("CRW_HOME", t.TempDir())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	dirPath := filepath.Dir(expectedReceiptPath(root))
	receiptPublish = publishThenFailSync(func() {
		receiptMust(t, os.Chmod(dirPath, 0o555))
		cancel()
	})
	defer func() { receiptPublish = crwdir.PublishContext }()
	t.Cleanup(func() { _ = os.Chmod(dirPath, 0o755) })
	a := ReceiptCLIArgs{Verb: "test", Cwd: root, Session: "s1", Command: receiptCommand(t, "exit", "0")}
	_, err := RunReceiptCLI(a, ReceiptRunOptions{Context: ctx, Stdout: io.Discard, Stderr: io.Discard})
	var pathErr *os.PathError
	if !errors.Is(err, syscall.EIO) || !errors.As(err, &pathErr) || pathErr.Op != "unlink" {
		t.Fatalf("error = %v, want the sync error joined with the unlink failure", err)
	}
	if _, statErr := os.Stat(expectedReceiptPath(root)); statErr != nil {
		t.Fatalf("the receipt is gone although its withdrawal failed: %v", statErr)
	}
}
