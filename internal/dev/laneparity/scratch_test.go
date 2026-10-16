//go:build dev

package laneparity

import (
	"os"
	"strings"
	"testing"
)

// CRW-1579 (D7): a run's scratch takes its parent from tmploc, the one place. A relative TMPDIR is
// refused with tmploc's reason, and a relative --scratch parent is refused too, so neither makes the
// run's directory below the working directory.
func TestMakeScratch_relativeTMPDIRIsAnError(t *testing.T) {
	cwd := t.TempDir()
	t.Chdir(cwd)
	t.Setenv("TMPDIR", "rel-tmp")
	if dir, err := makeScratch("", "crw-parity-"); err == nil || !strings.Contains(err.Error(), "tmpdir_not_absolute") {
		t.Errorf("makeScratch with TMPDIR=rel-tmp = %q, %v; want the tmpdir_not_absolute refusal", dir, err)
	}
	if dir, err := makeScratch("rel-scratch", "crw-parity-"); err == nil || !strings.Contains(err.Error(), "temp_parent_not_absolute") {
		t.Errorf("makeScratch(rel-scratch) = %q, %v; want the temp_parent_not_absolute refusal", dir, err)
	}
	if entries, _ := os.ReadDir(cwd); len(entries) != 0 {
		t.Errorf("a refused scratch made %d entries below the working directory", len(entries))
	}
}

// An absolute TMPDIR is the parent of the run's directory when no --scratch parent is given.
func TestMakeScratch_defaultParentIsTMPDIR(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)
	dir, err := makeScratch("", "crw-parity-")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(dir, tmp+string(os.PathSeparator)+"crw-parity-") {
		t.Errorf("makeScratch = %q, want a directory below TMPDIR %q", dir, tmp)
	}
}

// A TMPDIR that is absolute but cannot hold a directory is refused with tmploc's reason, naming the
// directory and its source; nothing else is tried.
func TestMakeScratch_unusableTMPDIRNamesItsSource(t *testing.T) {
	file := t.TempDir() + "/not-a-directory"
	if err := os.WriteFile(file, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TMPDIR", file)
	if dir, err := makeScratch("", "crw-parity-"); err == nil || !strings.Contains(err.Error(), "temp_dir_unusable: "+file+" (env)") {
		t.Errorf("makeScratch with TMPDIR=%s = %q, %v; want temp_dir_unusable naming it and its source", file, dir, err)
	}
}
