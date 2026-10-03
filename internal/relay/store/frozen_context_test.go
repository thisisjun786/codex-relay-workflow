package store

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A frozen copy is verified within the caller's context: a context that is already done ends each
// blob read at once instead of hashing the blob, so a daemon that is stopping is not kept waiting
// by the frozen fallback of a manifest check.
func TestVerifyFrozenEndsWithItsContext(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	artifact := filepath.Join(root, "deliverable.txt")
	if err := os.WriteFile(artifact, []byte("the delivered bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
	entries, err := BuildManifest(context.Background(), []string{artifact}, []string{root})
	if err != nil {
		t.Fatal(err)
	}
	reference := filepath.Join(root, "frozen")
	if err := FreezeManifest(entries, reference); err != nil {
		t.Fatal(err)
	}
	if problems, err := VerifyFrozen(context.Background(), reference, entries); err != nil || len(problems) != 0 {
		t.Fatalf("a live context verifies the copy: problems %v, error %v", problems, err)
	}
	done, cancel := context.WithCancel(context.Background())
	cancel()
	// Every blob read ends at once with the cancellation, which the verification reports as the
	// copy being unreadable; nothing is hashed.
	problems, err := VerifyFrozen(done, reference, entries)
	if err != nil || len(problems) != 1 || !strings.Contains(problems[0], context.Canceled.Error()) {
		t.Errorf("VerifyFrozen under a cancelled context: problems %v, error %v; want the one blob reported unreadable by the cancellation", problems, err)
	}
	_, _, unreadable, err := VerifyFrozenDetailed(done, reference, entries)
	if err != nil || len(unreadable) != 1 || !strings.Contains(unreadable[0], context.Canceled.Error()) {
		t.Errorf("VerifyFrozenDetailed under a cancelled context: unreadable %v, error %v", unreadable, err)
	}
}
