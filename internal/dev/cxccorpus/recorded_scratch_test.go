//go:build dev

package cxccorpus

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// CRW-1579 (D7, E5): RecordedScratch is the recorded case-root parent of the path_length_dependent
// fixtures. A case root below it, named by NewCase, is the 33 bytes contract/notes/cxc/README.md
// records ("/var/tmp/cxc-rec-<16 hex>, the recorded 33 bytes").
func TestRecordedScratchIsTheRecordedLength(t *testing.T) {
	const recorded = 33
	scratch := t.TempDir()
	c, err := NewCase(scratch, "CRW_HOME", Given{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(c.Root) })
	if filepath.Dir(c.Root) != scratch {
		t.Fatalf("NewCase made %q, not a child of %q", c.Root, scratch)
	}
	tail := len(c.Root) - len(scratch) // "/cxc-rec-<16 hex>"
	if got := len(RecordedScratch) + tail; got != recorded {
		t.Errorf("a case root below RecordedScratch %q is %d bytes (%d + %d), want the recorded %d", RecordedScratch, got, len(RecordedScratch), tail, recorded)
	}
	if got := len(RecordedScratch) + 1 + len("cxc-rec-") + 16; got != recorded {
		t.Errorf("len(RecordedScratch)+1+len(\"cxc-rec-\")+16 = %d, want %d", got, recorded)
	}
	root, err := RepositoryRoot()
	if err != nil {
		t.Fatal(err)
	}
	notes, err := os.ReadFile(filepath.Join(root, "contract", "notes", "cxc", "README.md"))
	if err != nil {
		t.Fatal(err)
	}
	if want := "`" + RecordedScratch + "/cxc-rec-<16 hex>`, the recorded 33 bytes"; !strings.Contains(string(notes), want) {
		t.Errorf("contract/notes/cxc/README.md no longer records %q", want)
	}
}
