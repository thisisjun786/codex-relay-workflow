package testsupport

import (
	"os"
	"path/filepath"
	"testing"
)

func TestA_relative_binary_override_keeps_naming_one_file_after_a_test_changes_directory(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "crw")
	if err := os.WriteFile(bin, []byte("stand-in"), 0o755); err != nil {
		t.Fatal(err)
	}
	relative, err := filepath.Rel(startDir, bin)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv(CRWBinaryEnv, relative)
	first, err := CRWPath()
	if err != nil {
		t.Fatal(err)
	}
	t.Chdir(t.TempDir())
	second, err := CRWPath()
	if err != nil {
		t.Fatal(err)
	}
	if first != bin || second != bin {
		t.Fatalf("override %q resolved to %q, then %q after a chdir; want %q", relative, first, second, bin)
	}
}
