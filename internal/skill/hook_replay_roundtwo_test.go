package skill

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestHookProbeUnreadableFixtureLivePython(t *testing.T) {
	// Given a copied fixture whose permissions prohibit reads by this user.
	binary := recordedCRW(t)
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
	// When the real command replays that unreadable fixture.
	answer := runHookProbeGo(t, binary, args...)
	// Then it may not silently skip the unreadable record.
	if answer.exit != 3 || !strings.Contains(answer.stderr, "Permission denied") {
		t.Fatalf("replay of an unreadable fixture: %+v", answer)
	}
}
