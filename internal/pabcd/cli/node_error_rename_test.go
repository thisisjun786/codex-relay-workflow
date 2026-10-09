package cli

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// The divergence mode write ends in a rename (crwdir.Rename), whose failure is a *os.LinkError and not a *os.PathError. Node
// spells a failed rename "<NAME>: <description>, rename '<old>' -> '<new>'" (CRW-749), and the original error stays reachable.
func TestNodeSpelledRenameFailure(t *testing.T) {
	dir := t.TempDir()
	missing, final := filepath.Join(dir, "mode.json.1.2.tmp"), filepath.Join(dir, "mode.json")
	err := os.Rename(missing, final)
	var link *os.LinkError
	if !errors.As(err, &link) {
		t.Fatalf("rename of a missing file = %v, want a *os.LinkError", err)
	}
	got := nodeSpelled(err)
	if want := "ENOENT: no such file or directory, rename '" + missing + "' -> '" + final + "'"; got.Error() != want {
		t.Errorf("a missing source reads %q, want %q", got.Error(), want)
	}
	if !errors.As(got, &link) || !errors.Is(got, os.ErrNotExist) {
		t.Errorf("the spelled error no longer unwraps to the *os.LinkError: %v", got)
	}
}

func TestNodeSpelledRenameRefusedByTheDirectory(t *testing.T) {
	dir := t.TempDir()
	tmp, final := filepath.Join(dir, "mode.json.1.2.tmp"), filepath.Join(dir, "mode.json")
	if err := os.WriteFile(tmp, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })
	err := os.Rename(tmp, final)
	if err == nil {
		t.Skip("the directory stays writable despite mode 0555 (privileged user)")
	}
	want := "EACCES: permission denied, rename '" + tmp + "' -> '" + final + "'"
	if got := nodeSpelled(err); got.Error() != want || !errors.Is(got, os.ErrPermission) {
		t.Errorf("a refused rename reads %q (permission %v), want %q", got.Error(), errors.Is(got, os.ErrPermission), want)
	}
}
