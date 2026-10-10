package testsupport

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The homes are the test's own: the process variables name them, and nothing in them to start with.
func TestSandboxAccountHomes_points_the_homes_at_temporary_directories(t *testing.T) {
	h := SandboxAccountHomes(t)
	for name, want := range map[string]string{"HOME": h.Home, "CODEX_HOME": h.Codex, "CRW_HOME": h.CRW} {
		if got := os.Getenv(name); got != want {
			t.Errorf("%s=%q, want %q", name, got, want)
		}
	}
	if err := os.MkdirAll(filepath.Join(h.Home, ".codex"), 0o700); err != nil {
		t.Fatal(err)
	}
	h.Rebase()
	h.Verify(func(msg string) { t.Errorf("an untouched home is reported: %s", msg) })
}

// A write below any of the four directories is reported, and a file another process creates in the real
// account home, which the sandbox never observes, is not.
func TestSandboxAccountHomes_reports_a_write_below_the_homes_and_ignores_the_real_home(t *testing.T) {
	real := t.TempDir()
	if err := os.MkdirAll(filepath.Join(real, ".codex"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", real)
	h := SandboxAccountHomes(t)
	// Given: a Codex session of the host opens its sqlite files in the real home mid-run.
	for _, name := range []string{"logs_2.sqlite-shm", "logs_2.sqlite-wal"} {
		if err := os.WriteFile(filepath.Join(real, ".codex", name), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// Then: the sandbox does not move.
	h.Verify(func(msg string) { t.Errorf("a file in the real home is reported: %s", msg) })
	// When: the code under test writes below each directory.
	for _, dir := range []string{filepath.Join(h.Home, ".codex"), filepath.Join(h.Home, ".crw"), h.Codex, h.CRW} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		reported := ""
		if err := os.WriteFile(filepath.Join(dir, "stray"), nil, 0o600); err != nil {
			t.Fatal(err)
		}
		h.Verify(func(msg string) { reported = msg })
		if !strings.Contains(reported, "stray") {
			t.Errorf("a write below %s is not reported: %q", dir, reported)
		}
		if err := os.Remove(filepath.Join(dir, "stray")); err != nil {
			t.Fatal(err)
		}
	}
	h.Rebase()
}
