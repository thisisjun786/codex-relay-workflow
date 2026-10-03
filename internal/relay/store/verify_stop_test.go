package store

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// A context that ends while a receipt's manifest is verified is a stop, not a refusal of the
// manifest: the reads it ended come back as problems, and the intake answers the context's error
// instead of a manifest_unverified refusal about bytes it never judged.
func TestVerifyBytesTakesAStopForAStop(t *testing.T) {
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
}
