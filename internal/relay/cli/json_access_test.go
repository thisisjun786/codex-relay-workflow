package cli_test

import (
	"context"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// Test24JSONAccessBytes uses the installed Python console entry point, a real gh
// process and a byte-identical SQLite seed per invocation. The driver compares
// stdout/stderr/exit and all 85 tables, including stored JSON text without parsing.
func Test24JSONAccessBytes(t *testing.T) {
	root, err := filepath.Abs("../../..")
	if err != nil {
		t.Fatal(err)
	}
	binary, _ := packageBinary(t)
	// CI runs the mutation-backed representatives, without race/short skips.
	// Run testdata/json_access.py without --representative for the full matrix.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, filepath.Join(root, ".venv/bin/python"), "testdata/json_access.py", "--root", root, "--binary", binary, "--representative")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("JSON accessor parity: %v\n%s", err, out)
	} else {
		t.Log(string(out))
	}
}
