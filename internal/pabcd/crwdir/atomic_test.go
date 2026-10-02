package crwdir

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

func TestRenameReplacesTheDestinationAndSurfacesAFailure(t *testing.T) {
	dir := t.TempDir()
	tmp, final := filepath.Join(dir, "tmp"), filepath.Join(dir, "final")
	for path, text := range map[string]string{tmp: "new", final: "old"} {
		if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := Rename(tmp, final); err != nil || read(t, final) != "new" {
		t.Fatalf("Rename = %v, final %q", err, read(t, final))
	}
	if err := os.WriteFile(tmp, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := Rename(tmp, filepath.Join(dir, "missing", "final")); !errors.Is(err, os.ErrNotExist) || read(t, tmp) != "x" {
		t.Fatalf("a failed rename: %v, source %q", err, read(t, tmp))
	}
}

// atomic-write.test.ts "POSIX never retries": even EBUSY, which only win32 treats as transient, is
// returned after exactly one attempt.
func TestRenameNeverRetries(t *testing.T) {
	calls := 0
	err := renameWith(func(string, string) error { calls++; return syscall.EBUSY }, "tmp", "final")
	if !errors.Is(err, syscall.EBUSY) || calls != 1 {
		t.Fatalf("err = %v after %d calls", err, calls)
	}
}
