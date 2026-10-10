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
// retarget of the link to a twin directory with the same contents and modes is reported by the link's target.
// Each of the three is the only link of its own subtest, so each is shown to be walked on its own.
func TestAccountHomesSnapshot_follows_a_watched_directory_that_is_a_symlink(t *testing.T) {
	for _, which := range []string{"HOME/.codex", "CODEX_HOME", "CRW_HOME"} {
		t.Run(which, func(t *testing.T) {
			home, codex, crw := t.TempDir(), t.TempDir(), t.TempDir()
			real, twin := t.TempDir(), t.TempDir()
			const original, edited = "model = \"a\"\n", "model = \"b\"\n"
			for _, dir := range []string{real, twin} {
				if err := os.WriteFile(filepath.Join(dir, "config.toml"), []byte(original), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			var link string
			switch which {
			case "HOME/.codex":
				link = filepath.Join(home, ".codex")
			case "CODEX_HOME":
				link = filepath.Join(t.TempDir(), "codex-link")
				codex = link
			case "CRW_HOME":
				link = filepath.Join(t.TempDir(), "crw-link")
				crw = link
			}
			if err := os.Symlink(real, link); err != nil {
				t.Fatal(err)
			}
			h := WatchAccountHomes(t, home, codex, crw)
			h.Snapshot()
			h.Verify(func(msg string) { t.Errorf("an untouched fixture behind the link is reported: %s", msg) })

			// Given: the run edits the fixture behind the link in place, same name and same size.
			fixture := filepath.Join(real, "config.toml")
			if err := os.WriteFile(fixture, []byte(edited), 0o600); err != nil {
				t.Fatal(err)
			}
			reported := ""
			h.Verify(func(msg string) { reported = msg })
			if !strings.Contains(reported, "config.toml") {
				t.Errorf("an in-place edit behind the %s link is not reported: %q", which, reported)
			}
			if err := os.WriteFile(fixture, []byte(original), 0o600); err != nil {
				t.Fatal(err)
			}
			h.Verify(func(msg string) { t.Errorf("the restored fixture is reported: %s", msg) })

			// Then: pointing the link at the twin, same contents and modes, is reported.
			if err := os.Remove(link); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(twin, link); err != nil {
				t.Fatal(err)
			}
			reported = ""
			h.Verify(func(msg string) { reported = msg })
			if !strings.Contains(reported, "-> "+twin) {
				t.Errorf("a retarget of the %s link to a twin directory is not reported: %q", which, reported)
			}
			if err := os.Remove(link); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(real, link); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// CRW-1176: an entry the snapshot cannot read is no state to compare. A fixture the snapshot cannot read ends
// neither the walk nor the check: a later fixture's in-place edit and mode change are still seen, and Verify
// fails on the unreadable entry even when the run changed nothing, since the same error before and after hides
// whatever stands behind it.
func TestAccountHomesSnapshot_fails_on_an_entry_it_cannot_read(t *testing.T) {
	codex := t.TempDir()
	blocked := filepath.Join(codex, "a-unreadable")
	fixture := filepath.Join(codex, "b-fixture.toml")
	if err := os.WriteFile(blocked, []byte("secret"), 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(blocked, 0o600) })
	if _, err := os.ReadFile(blocked); err == nil {
		t.Skip("the process reads a mode 0000 file (root); the unreadable entry cannot be made")
	}
	const original, edited = "status = \"ACTIVE\"\n", "status = \"PAUSED\"\n"
	if err := os.WriteFile(fixture, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	// The homes are built here, not by WatchAccountHomes: its cleanup would report the unreadable entry again.
	h := &AccountHomes{Home: t.TempDir(), Codex: codex, CRW: t.TempDir(), watchHome: true}
	h.Snapshot()

	// Then: the unreadable entry fails the check although nothing changed.
	reported := ""
	h.Verify(func(msg string) { reported = msg })
	if !strings.Contains(reported, "a-unreadable") {
		t.Errorf("an unreadable entry passes the check: %q", reported)
	}

	// Given: the run edits the later fixture in place and changes its mode.
	if err := os.WriteFile(fixture, []byte(edited), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(fixture, 0o644); err != nil {
		t.Fatal(err)
	}
	// Then: the change behind the unreadable entry is seen.
	reported = ""
	h.Verify(func(msg string) { reported = msg })
	if !strings.Contains(reported, "b-fixture.toml -rw-r--r--") {
		t.Errorf("a fixture change after an unreadable entry is not seen: %q", reported)
	}
}

// CRW-1176: a watched directory can be spelled through a symlink followed by "..", which the kernel resolves
// against the link's target (base/work/alias/../codex is storage/codex when alias points into storage), while
// filepath.Join folds the ".." lexically (base/work/codex). The snapshot reads the directory the kernel
// resolves: an unchanged physical home is silent whether or not its lexical counterpart exists, an in-place
// edit of a physical fixture is reported, and an edit of a lexical twin is not.
func TestAccountHomesSnapshot_reads_the_physical_directory_of_a_root_spelled_through_a_link_and_dotdot(t *testing.T) {
	for _, lexicalExists := range []bool{false, true} {
		name := "lexical counterpart absent"
		if lexicalExists {
			name = "lexical counterpart present"
		}
		t.Run(name, func(t *testing.T) {
			base := t.TempDir()
			storage := filepath.Join(base, "storage")
			physical := filepath.Join(storage, "codex")
			lexical := filepath.Join(base, "work", "codex")
			for _, dir := range []string{filepath.Join(storage, "project"), physical, filepath.Join(base, "work")} {
				if err := os.MkdirAll(dir, 0o755); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.Symlink(filepath.Join(storage, "project"), filepath.Join(base, "work", "alias")); err != nil {
				t.Fatal(err)
			}
			if lexicalExists {
				if err := os.MkdirAll(lexical, 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(lexical, "config.toml"), []byte("model = \"lexical\"\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			const original, edited = "model = \"a\"\n", "model = \"b\"\n"
			fixture := filepath.Join(physical, "config.toml")
			if err := os.WriteFile(fixture, []byte(original), 0o600); err != nil {
				t.Fatal(err)
			}
			// Spelled by concatenation: filepath.Join would fold the ".." before the kernel sees it.
			codex := base + "/work/alias/../codex"
			h := WatchAccountHomes(t, t.TempDir(), codex, t.TempDir())
			h.Snapshot()
			h.Verify(func(msg string) { t.Errorf("an unchanged physical home is reported: %s", msg) })

			if err := os.WriteFile(fixture, []byte(edited), 0o600); err != nil {
				t.Fatal(err)
			}
			reported := ""
			h.Verify(func(msg string) { reported = msg })
			if !strings.Contains(reported, "config.toml") {
				t.Errorf("an in-place edit of the physical fixture is not reported: %q", reported)
			}
			if err := os.WriteFile(fixture, []byte(original), 0o600); err != nil {
				t.Fatal(err)
			}
			if lexicalExists {
				if err := os.WriteFile(filepath.Join(lexical, "config.toml"), []byte("model = \"changed\"\n"), 0o600); err != nil {
					t.Fatal(err)
				}
				h.Verify(func(msg string) { t.Errorf("an edit of the lexical twin is reported: %s", msg) })
			}
		})
	}
}
