package cli

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/state"
)

// linkRefuseListing is every entry below dir with its content, so a test can show that nothing outside the workspace changed.
func linkRefuseListing(t *testing.T, dir string) string {
	t.Helper()
	var out []string
	err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err == nil && path != dir {
			data, _ := os.ReadFile(path)
			out = append(out, strings.TrimPrefix(path, dir)+"="+string(data))
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return strings.Join(out, "\n")
}

// plantLink puts a symbolic link to outside at cwd/rel, making the directories above it.
func plantLink(t *testing.T, cwd, rel, outside string) string {
	t.Helper()
	link := filepath.Join(cwd, rel)
	receiptMust(t, os.MkdirAll(filepath.Dir(link), 0o777))
	receiptMust(t, os.Symlink(outside, link))
	return link
}

// scan record with a link at the scan ledger's directory or file is an input error: exit 1, a message that names the link as a
// symbolic link, nothing written where it leads and the tracker not advanced.
func TestScanRecordRefusesALinkedLedger(t *testing.T) {
	for _, rel := range []string{".crw/interviews", ".crw/interviews/s1.jsonl"} {
		t.Run(rel, func(t *testing.T) {
			cwd, outside := t.TempDir(), t.TempDir()
			receiptMust(t, state.WriteState(cwd, state.DefaultState("s1", "")))
			target := outside
			if strings.HasSuffix(rel, ".jsonl") {
				target = filepath.Join(outside, "victim")
				receiptMust(t, os.WriteFile(target, []byte("keep\n"), 0o644))
			}
			link := plantLink(t, cwd, rel, target)
			before := linkRefuseListing(t, outside)
			got := RunScanCli(ScanCliArgs{Action: ScanActionRecord, SessionID: "s1", Cwd: cwd})
			if got.Code != 1 || !strings.Contains(got.Output, link) || !strings.Contains(got.Output, "symbolic link") {
				t.Fatalf("got %+v, want exit 1 naming %s as a symbolic link", got, link)
			}
			if after := linkRefuseListing(t, outside); after != before {
				t.Fatalf("outside the workspace changed:\n%s\nwas\n%s", after, before)
			}
			if s := state.ReadState(cwd, "s1"); s.Interview != nil && s.Interview.ScanRounds != 0 {
				t.Fatalf("the tracker advanced: %+v", s.Interview)
			}
		})
	}
}

// receipt test with a link at the evidence directory or the session's receipt directory is refused: the old receipt the link
// leads to is neither removed nor replaced, and no receipt is published there.
func TestReceiptRefusesALinkedEvidenceDirectory(t *testing.T) {
	for _, rel := range []string{".crw/evidence", ".crw/evidence/s1"} {
		t.Run(rel, func(t *testing.T) {
			root, outside := receiptRepo(t), t.TempDir()
			old := filepath.Join(outside, "s1", "test-receipt.json")
			if rel == ".crw/evidence/s1" {
				old = filepath.Join(outside, "test-receipt.json")
			}
			receiptMust(t, os.MkdirAll(filepath.Dir(old), 0o777))
			receiptMust(t, os.WriteFile(old, []byte("someone else's"), 0o644))
			link := plantLink(t, root, rel, outside)
			before := linkRefuseListing(t, outside)
			a := ReceiptCLIArgs{Verb: "test", Cwd: root, Session: "s1", Command: receiptCommand(t, "exit", "0")}
			got, err := RunReceiptCLI(a, ReceiptRunOptions{Stdout: io.Discard, Stderr: io.Discard})
			if err == nil || !strings.Contains(err.Error(), link) || !strings.Contains(err.Error(), "symbolic link") {
				t.Fatalf("got %+v, %v; want an error naming %s as a symbolic link", got, err, link)
			}
			if after := linkRefuseListing(t, outside); after != before {
				t.Fatalf("outside the workspace changed:\n%s\nwas\n%s", after, before)
			}
		})
	}
}
