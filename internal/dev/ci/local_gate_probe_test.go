//go:build dev

package ci

import (
	"io"
	"os"
	"path/filepath"
	"testing"
)

// CRW-964 pre-merge finding d1: the tool-version probes that feed the record and the reuse keys run
// through the heavy-check gate when one is configured, as the checks they judge do.
func TestLocalTools_the_version_probes_run_through_the_heavy_gate(t *testing.T) {
	repo := newLocalFixture(t)
	dir := t.TempDir()
	mark := filepath.Join(dir, "gate-calls")
	gate := filepath.Join(dir, "gate.sh")
	// The probe runs in the sealed environment, so the mark path is written into the script itself.
	script := "#!/bin/sh\necho \"$*\" >> " + mark + "\nexec \"$@\"\n"
	if err := os.WriteFile(gate, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	opts := localRunOptions(repo, localFixturePlan("echo hello"), filepath.Join(dir, "record.json"))
	opts.HeavyGate = gate
	if _, err := localCurrentKeys(opts); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(mark)
	if err != nil || len(data) == 0 {
		t.Errorf("no tool probe ran through the heavy-check gate (%v)", err)
	}
}

var _ = io.Discard
