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

// CRW-1170: a test that lists the account's real ~/.codex or ~/.crw fails whenever another process of the host
// writes there (a live Codex session opens its sqlite files). The real home is reached through os.UserHomeDir
// in a test binary that does not isolate HOME; no test file may ask for it. A test that needs a home uses
// SandboxAccountHomes, or reads the HOME the isolation gave it.
func TestNoTestFileResolvesTheAccountHome(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	self := filepath.Join("internal", "testsupport", "homes_test.go")
	needle := "os.User" + "HomeDir("
	err = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		if d.IsDir() && (d.Name() == ".git" || d.Name() == "node_modules" || strings.HasPrefix(rel, ".omo")) {
			return filepath.SkipDir
		}
		if d.IsDir() || !strings.HasSuffix(path, "_test.go") || rel == self {
			return nil
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if strings.Contains(string(raw), needle) {
			t.Errorf("%s resolves the account home; use SandboxAccountHomes", rel)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// A watch reports a write below the directories a child process was given, the top level of its HOME included,
// and sets no process variable.
func TestWatchAccountHomes_reports_a_write_below_the_homes_it_was_given(t *testing.T) {
	t.Setenv("CODEX_HOME", "kept")
	home, codex, crw := t.TempDir(), t.TempDir(), t.TempDir()
	h := WatchAccountHomes(t, home, codex, crw)
	if os.Getenv("CODEX_HOME") != "kept" {
		t.Errorf("the watch changed CODEX_HOME to %q", os.Getenv("CODEX_HOME"))
	}
	h.Verify(func(msg string) { t.Errorf("an untouched home is reported: %s", msg) })
	for _, dir := range []string{home, codex, crw} {
		reported := ""
		stray := filepath.Join(dir, "stray")
		if err := os.WriteFile(stray, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		h.Verify(func(msg string) { reported = msg })
		if !strings.Contains(reported, "stray") {
			t.Errorf("a write below %s is not reported: %q", dir, reported)
		}
		if err := os.Remove(stray); err != nil {
			t.Fatal(err)
		}
	}
}

// A watch given no codex directory still reports a write below HOME, HOME/.codex, HOME/.crw and crw, and does
// not take the empty name for the working directory.
func TestWatchAccountHomes_leaves_an_unnamed_home_to_the_test(t *testing.T) {
	home, crw := t.TempDir(), t.TempDir()
	h := WatchAccountHomes(t, home, "", crw)
	if parts := strings.Split(h.Listing(), " | "); len(parts) != 4 {
		t.Errorf("the listing has %d parts, want HOME, /.codex, /.crw and crw: %s", len(parts), h.Listing())
	}
	for _, dir := range []string{home, filepath.Join(home, ".codex"), filepath.Join(home, ".crw"), crw} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		h.Rebase()
		reported := ""
		stray := filepath.Join(dir, "stray")
		if err := os.WriteFile(stray, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		h.Verify(func(msg string) { reported = msg })
		if !strings.Contains(reported, "stray") {
			t.Errorf("a write below %s is not reported: %q", dir, reported)
		}
		if err := os.Remove(stray); err != nil {
			t.Fatal(err)
		}
	}
	h.Rebase()
}

// CRW-1176: a fixture edited in place keeps its name and its place, so the name-only listing does not move. A
// home that holds fixtures takes the content snapshot instead, and a change to a fixture's bytes is reported.
func TestAccountHomesSnapshot_reports_an_in_place_fixture_edit(t *testing.T) {
	h := SandboxAccountHomes(t)
	dir := filepath.Join(h.Codex, "automations", "one")
	fixture := filepath.Join(dir, "automation.toml")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	const original, edited = "status = \"ACTIVE\"\n", "status = \"PAUSED\"\n"
	if err := os.WriteFile(fixture, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	h.Snapshot()
	// Given: the run edits the fixture in place, same name and same size.
	if err := os.WriteFile(fixture, []byte(edited), 0o600); err != nil {
		t.Fatal(err)
	}
	// Then: the edit is reported.
	reported := ""
	h.Verify(func(msg string) { reported = msg })
	if !strings.Contains(reported, "automation.toml") {
		t.Errorf("an in-place edit of a fixture is not reported: %q", reported)
	}
	if err := os.WriteFile(fixture, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
}

// A snapshot of untouched fixtures reports nothing, a chmod of a fixture is reported, and a fixture the run adds
// below a watched directory is reported by name.
func TestAccountHomesSnapshot_stays_silent_until_a_fixture_changes(t *testing.T) {
	h := SandboxAccountHomes(t)
	dir := filepath.Join(h.Codex, "automations", "one")
	fixture := filepath.Join(dir, "automation.toml")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(fixture, []byte("status = \"ACTIVE\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	h.Snapshot()
	h.Verify(func(msg string) { t.Errorf("an untouched fixture is reported: %s", msg) })

	reported := ""
	if err := os.Chmod(fixture, 0o644); err != nil {
		t.Fatal(err)
	}
	h.Verify(func(msg string) { reported = msg })
	if !strings.Contains(reported, "automation.toml") {
		t.Errorf("a mode change of a fixture is not reported: %q", reported)
	}
	if err := os.Chmod(fixture, 0o600); err != nil {
		t.Fatal(err)
	}

	reported = ""
	stray := filepath.Join(dir, "stray")
	if err := os.WriteFile(stray, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	h.Verify(func(msg string) { reported = msg })
	if !strings.Contains(reported, "stray") {
		t.Errorf("a fixture added below a watched directory is not reported: %q", reported)
	}
	if err := os.Remove(stray); err != nil {
		t.Fatal(err)
	}
}

// CRW-1176: a watched directory can itself be a symlink to a directory (HOME/.codex, CODEX_HOME and CRW_HOME all
// can). The walk follows the link at its root, so an in-place edit of a fixture behind it is reported, and a
// retarget of the link is reported too.
func TestAccountHomesSnapshot_follows_a_watched_directory_that_is_a_symlink(t *testing.T) {
	home := t.TempDir()
	real := t.TempDir()
	other := t.TempDir()
	codexLink := filepath.Join(t.TempDir(), "codex-link")
	if err := os.Symlink(real, codexLink); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(real, filepath.Join(home, ".codex")); err != nil {
		t.Fatal(err)
	}
	fixture := filepath.Join(real, "config.toml")
	const original, edited = "model = \"a\"\n", "model = \"b\"\n"
	if err := os.WriteFile(fixture, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	h := WatchAccountHomes(t, home, codexLink, "")
	h.Snapshot()
	h.Verify(func(msg string) { t.Errorf("an untouched fixture behind a link is reported: %s", msg) })

	// Given: the run edits the fixture behind the link in place, same name and same size.
	if err := os.WriteFile(fixture, []byte(edited), 0o600); err != nil {
		t.Fatal(err)
	}
	reported := ""
	h.Verify(func(msg string) { reported = msg })
	if !strings.Contains(reported, "config.toml") {
		t.Errorf("an in-place edit behind a watched directory link is not reported: %q", reported)
	}
	if err := os.WriteFile(fixture, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	h.Verify(func(msg string) { t.Errorf("the restored fixture is reported: %s", msg) })

	// Then: pointing the link elsewhere is reported.
	if err := os.Remove(codexLink); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(other, codexLink); err != nil {
		t.Fatal(err)
	}
	reported = ""
	h.Verify(func(msg string) { reported = msg })
	if reported == "" {
		t.Errorf("a retargeted watched directory link is not reported")
	}
	if err := os.Remove(codexLink); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(real, codexLink); err != nil {
		t.Fatal(err)
	}
}
