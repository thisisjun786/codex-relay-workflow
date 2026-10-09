package reset

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"testing"
)

// CRW-1094 (known-defects.md:79): a writer killed between its temp write and the rename leaves
// <id>.json.<pid>.<suffix>.tmp behind for good. The explicit maintenance command removes such a file
// once its writer is gone; a temp file whose writer is alive, and a name the writers never make, stay.
// The hooks never sweep.
func TestResetStateRemovesOrphanedStateTempsOfGoneWriters(t *testing.T) {
	cwd := t.TempDir()
	sessions := filepath.Join(cwd, ".crw", "sessions")
	if err := os.MkdirAll(sessions, 0o755); err != nil {
		t.Fatal(err)
	}
	dead := exec.Command("true")
	if err := dead.Run(); err != nil {
		t.Fatal(err)
	}
	gone, live := strconv.Itoa(dead.Process.Pid), strconv.Itoa(os.Getpid())
	orphans := []string{
		"s.json." + gone + ".0f1e2d3c-4b5a-4968-8776-655443322110.tmp", // the port's writer
		"s.json." + gone + ".1767225600000.tmp",                        // the oracle's writer
	}
	kept := []string{
		"s.json." + live + ".0f1e2d3c-4b5a-4968-8776-655443322110.tmp", // a writer that is alive
		"notes.tmp",               // no writer's name
		"s.json.x.1.tmp",          // no pid
		"s.json." + gone + ".tmp", // no suffix
	}
	for _, name := range append(append([]string{"s.json"}, orphans...), kept...) {
		if err := os.WriteFile(filepath.Join(sessions, name), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Mkdir(filepath.Join(sessions, "d.json."+gone+".1.tmp"), 0o755); err != nil { // not a file: left alone
		t.Fatal(err)
	}
	result, err := RunReset(cwd, State)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range append([]string{"s.json"}, orphans...) {
		if !slices.Contains(result.Removed, filepath.Join(cwd, ".crw", "sessions", name)) {
			t.Errorf("%s not reported removed: %v", name, result.Removed)
		}
		if _, err := os.Lstat(filepath.Join(sessions, name)); !os.IsNotExist(err) {
			t.Errorf("%s still there: %v", name, err)
		}
	}
	for _, name := range append(kept, "d.json."+gone+".1.tmp") {
		if _, err := os.Lstat(filepath.Join(sessions, name)); err != nil {
			t.Errorf("%s was removed: %v", name, err)
		}
	}
}
