package store

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport"
)

// A context that ends while a receipt's manifest is verified is a stop, not a refusal of the
// manifest: the reads it ended come back as problems, and the intake answers the context's error
// instead of a manifest_unverified refusal about bytes it never judged.
func TestVerifyBytesTakesAStopForAStop(t *testing.T) {
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
	frozen := filepath.Join(root, "frozen")
	if err := FreezeManifest(entries, frozen); err != nil {
		t.Fatal(err)
	}
	in := ReceiptIntake{}
	if _, err := in.verifyBytes(context.Background(), entries, []string{root}, &frozen); err != nil {
		t.Fatalf("a live context verifies the manifest: %v", err)
	}
	stopped, cancel := context.WithCancel(context.Background())
	cancel()
	for name, ref := range map[string]*string{"live copy only": nil, "with a frozen copy": &frozen} {
		_, err := in.verifyBytes(stopped, entries, []string{root}, ref)
		if !errors.Is(err, context.Canceled) {
			t.Errorf("%s: a stopped context answered %v, want the stop and no refusal of the manifest", name, err)
		}
	}

	// The live file now disagrees with the receipt and the frozen copy is what verifies it, so the
	// frozen fallback is on the path. The context ends after n checks, for every n: wherever it
	// ends, the answer is the manifest verified or the stop, and a refusal never appears.
	if err := os.WriteFile(artifact, []byte("bytes edited after the receipt"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := in.verifyBytes(context.Background(), entries, []string{root}, &frozen); err != nil {
		t.Fatalf("a live context falls back to the frozen copy and verifies the manifest: %v", err)
	}
	stops := 0
	for n := 0; n < 200; n++ {
		_, err := in.verifyBytes(testsupport.StopAfter(n), entries, []string{root}, &frozen)
		switch {
		case err == nil:
		case errors.Is(err, context.Canceled):
			stops++
		default:
			t.Fatalf("a context that ended after %d checks: %v; want the manifest verified or the stop", n, err)
		}
	}
	if stops == 0 {
		t.Fatal("no context ended inside the verification: the boundary was not reached")
	}
}
