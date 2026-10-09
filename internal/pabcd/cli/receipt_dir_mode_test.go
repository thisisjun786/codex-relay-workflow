package cli

import (
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A publish needs write and search permission on the evidence directory, as the publication itself does, and not read
// permission: the directory lock is a lock file inside it (CRW-1013, #598 CRW-636 evaluation P0). A directory at 0300 is
// the write-and-search-only case; a process that may read it would pass whatever the mode, so root skips.
func TestReceiptPublishInAWriteOnlyEvidenceDirectory(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root: root reads a directory whatever its mode, so the 0300 case cannot be reproduced")
	}
	root := receiptRepo(t)
	t.Setenv("CRW_HOME", t.TempDir())
	dir := filepath.Dir(expectedReceiptPath(root))
	receiptMust(t, os.MkdirAll(dir, 0o777))
	receiptMust(t, os.Chmod(dir, 0o300))
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })
	a := ReceiptCLIArgs{Verb: "test", Cwd: root, Session: "s1", Command: receiptCommand(t, "exit", "0")}
	if got := receiptRun(t, a, ReceiptRunOptions{}); got != (ReceiptCLIResult{Output: expectedReceiptPath(root)}) {
		t.Fatalf("publish in a write-only evidence directory = %#v, want the receipt path", got)
	}
	receiptMust(t, os.Chmod(dir, 0o755))
	if _, err := os.Stat(expectedReceiptPath(root)); err != nil {
		t.Fatalf("the receipt was not published: %v", err)
	}
}

// A directory without write permission refuses the publish with the error it always gave: the temporary file of the
// publication cannot be created there, and the message names that temporary file (CRW-1013 keeps this message).
func TestReceiptPublishInAReadOnlyEvidenceDirectoryKeepsItsRefusal(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root: root writes a read-only directory, so the refusal cannot be reproduced")
	}
	root := receiptRepo(t)
	t.Setenv("CRW_HOME", t.TempDir())
	dir := filepath.Dir(expectedReceiptPath(root))
	receiptMust(t, os.MkdirAll(dir, 0o777))
	receiptMust(t, os.Chmod(dir, 0o555))
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })
	a := ReceiptCLIArgs{Verb: "test", Cwd: root, Session: "s1", Command: receiptCommand(t, "exit", "0")}
	_, err := RunReceiptCLI(a, ReceiptRunOptions{Stdout: io.Discard, Stderr: io.Discard})
	if err == nil {
		t.Fatal("publish in a read-only evidence directory succeeded")
	}
	prefix, suffix := "open "+dir+"/.test-receipt.json.", ".tmp: permission denied"
	if msg := err.Error(); !strings.HasPrefix(msg, prefix) || !strings.HasSuffix(msg, suffix) || !errors.Is(err, fs.ErrPermission) {
		t.Fatalf("refusal = %q (permission %v), want %q...%q", msg, errors.Is(err, fs.ErrPermission), prefix, suffix)
	}
	if _, statErr := os.Stat(expectedReceiptPath(root)); !errors.Is(statErr, fs.ErrNotExist) {
		t.Fatalf("a refused publish left a receipt: %v", statErr)
	}
}
