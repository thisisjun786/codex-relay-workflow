package argparse

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func Test24GeneratedSpecDrift(t *testing.T) {
	root, err := filepath.Abs("../../..")
	if err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(t.TempDir(), "specs.json")
	cmd := exec.Command(filepath.Join(root, ".venv/bin/python"), "generate_spec.py", output)
	if raw, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("generate spec: %v\n%s", err, raw)
	}
	want, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(data, want) {
		for i := 0; i < min(len(data), len(want)); i++ {
			if data[i] != want[i] {
				t.Fatalf("spec byte diff at %d\ncommitted: %q\ngenerated: %q", i, data[max(0, i-80):min(len(data), i+160)], want[max(0, i-80):min(len(want), i+160)])
			}
		}
		t.Fatalf("spec byte diff: committed length=%d generated length=%d", len(data), len(want))
	}
}
