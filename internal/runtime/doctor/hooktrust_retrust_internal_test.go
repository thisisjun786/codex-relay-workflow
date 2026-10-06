package doctor

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestHookTrustRetrustReplace_refuses_a_changed_file covers the compare-and-swap before the write,
// the one guard the command line cannot reach in a test (nothing else writes between the read and
// the write). Its rollback twin is exercised end to end by
// TestHookTrustRetrust_keeps_a_concurrent_edit_on_rollback.
func TestHookTrustRetrustReplace_refuses_a_changed_file(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	original := "model = \"gpt-5.5\"\n"
	concurrent := "model = \"another-writer\"\n"
	if err := os.WriteFile(path, []byte(concurrent), 0o644); err != nil {
		t.Fatal(err)
	}
	err := hookTrustRetrustReplace(path, original, "model = \"rewritten\"\n")
	if err == nil || !strings.Contains(err.Error(), "changed while retrust was running") {
		t.Fatalf("a changed file was replaced: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != concurrent {
		t.Fatalf("the refused write changed the file: %q", got)
	}
}
