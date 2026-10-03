package dagsched

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// A context that ends while the frozen copy of a manifest is read is a stop, not evidence about the
// copy: the blob reads it ended come back as problems, and reading them as B-17 would block a
// successor node on a copy that is fine. The answer is the context's error, as it is for the files.
func TestFrozenFindingTakesAStopForAStop(t *testing.T) {
	root := t.TempDir()
	artifact := filepath.Join(root, "design.md")
	if err := os.WriteFile(artifact, []byte("the delivered design\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	declared, err := store.BuildManifest(context.Background(), []string{artifact}, []string{root})
	if err != nil {
		t.Fatal(err)
	}
	frozen := filepath.Join(root, "frozen")
	if err := store.FreezeManifest(declared, frozen); err != nil {
		t.Fatal(err)
	}
	if finding, err := frozenFinding(context.Background(), frozen, declared); finding != nil || err != nil {
		t.Fatalf("a good copy under a live context: finding %+v, error %v", finding, err)
	}
	stopped, cancel := context.WithCancel(context.Background())
	cancel()
	if finding, err := frozenFinding(stopped, frozen, declared); finding != nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("a good copy under a stopped context: finding %+v, error %v; want the stop and no B-17", finding, err)
	}
	if finding, err := frozenFinding(context.Background(), filepath.Join(root, "absent"), declared); err != nil || finding == nil || finding.Code != "B-17" {
		t.Fatalf("no copy under a live context: finding %+v, error %v; want B-17", finding, err)
	}
}
