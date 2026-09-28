package skill

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestHookProbeUnreadableFixtureLivePython(t *testing.T) {
	// Given a copied fixture whose permissions prohibit reads by this user.
	binary := buildHookProbeCLI(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "unreadable.json")
	if err := os.WriteFile(path, []byte(`{"observation":{},"expected":{"state":"unmanaged"}}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Chmod(path, 0600); err != nil {
			t.Error(err)
		}
	})
	if _, err := os.ReadFile(path); !os.IsPermission(err) {
		t.Fatalf("chmod 000 did not prevent reads: %v", err)
	}
	args := []string{"replay", "--fixtures", dir, "--allow-unreached"}
	// When both real commands replay that unreadable fixture.
	python := runHookProbePython(t, args...)
	if python.exit != 3 || !strings.Contains(python.stderr, "Permission denied") {
		t.Fatalf("Python permission oracle: %+v", python)
	}
	// Then neither may silently skip the unreadable record.
	requireHookProbeParity(t, python, runHookProbeGo(t, binary, args...))
}
