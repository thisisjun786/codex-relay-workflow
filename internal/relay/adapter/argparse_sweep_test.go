//go:build parity

package adapter

import (
	"bytes"
	"encoding/json"
	"os/exec"
	"path/filepath"
	"testing"
)

// Todo 24 owns the shared formatter. These are the actual adapter-integrated
// commands, replayed from Python's parser so the merge can enable the full matrix.
func Test28_ArgparseAdapterCommands(t *testing.T) {
	runArgparseSweep(t)
}
func runArgparseSweep(t *testing.T) {
	t.Helper()
	repo, _ := filepath.Abs("../../..")
	binary := suiteBinary
	command := exec.Command("uv", "run", "--no-sync", "python", filepath.Join(repo, "internal/relay/adapter/testdata/argparse_capture.py"), binary)
	command.Dir = repo
	out, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("argparse sweep %v\n%s", err, out)
	}
	var result map[string]any
	if err := json.NewDecoder(bytes.NewReader(out)).Decode(&result); err != nil {
		t.Fatal(err)
	}
	if result["differences"] != float64(0) {
		t.Fatalf("argparse mismatch %s", out)
	}
}
