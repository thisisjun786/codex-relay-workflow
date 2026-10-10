//go:build dev

package cxccorpus

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/dev/homeguard"
)

// CRW-1186: a case root is made below the scratch directory the caller names; a scratch directory in the
// account's real home (.codex, .crw, .local/share/crw-runtime) is refused before anything is made.
func TestNewCase_refusesAScratchInTheAccountHome(t *testing.T) {
	home := t.TempDir()
	t.Cleanup(homeguard.SetAccountHome(home))
	for _, rel := range []string{".codex", ".crw/scratch", ".local/share/crw-runtime/x"} {
		scratch := filepath.Join(home, rel)
		if err := os.MkdirAll(scratch, 0o755); err != nil {
			t.Fatal(err)
		}
		c, err := NewCase(scratch, "CRW_HOME", Given{})
		var refusal *homeguard.Error
		if !errors.As(err, &refusal) {
			t.Errorf("%s: want a refusal of the account home, got %v", rel, err)
		}
		if c != nil {
			t.Errorf("%s: a case came back at %s", rel, c.Root)
		}
		if entries, _ := os.ReadDir(scratch); len(entries) != 0 {
			t.Errorf("%s holds %d entries", rel, len(entries))
		}
	}
}

// CRW-1186: a link the scenario's given makes (Given.Symlinks) may lead into the account's real home;
// no write, directory, link, mode, time or repository the engine makes through it lands there. Each
// case is refused before the protected file or directory changes.
func TestRunScenario_writesNothingInTheAccountHomeThroughAGivenLink(t *testing.T) {
	git, _ := exec.LookPath("git")
	link := map[string]string{"ws/account": "${HOME_FAKE}/.codex"}
	for name, s := range map[string]Scenario{
		"step write":                       {Given: Given{Symlinks: link}, Steps: []Step{{Write: map[string]string{"ws/account/probe.json": "overwritten"}}}},
		"step write below a new directory": {Given: Given{Symlinks: link}, Steps: []Step{{Write: map[string]string{"ws/account/new/probe.json": "x"}}}},
		"mode of a file":                   {Given: Given{Symlinks: link, Modes: map[string]int{"ws/account/probe.json": 0o777}}},
		"mode of the link":                 {Given: Given{Symlinks: link, Modes: map[string]int{"ws/account": 0o777}}},
		"mtime":                            {Given: Given{Symlinks: link, Mtimes: map[string]string{"ws/account/probe.json": "2020-01-02T00:00:00Z"}}},
		"nested link":                      {Given: Given{Symlinks: map[string]string{"ws/account": "${HOME_FAKE}/.codex", "ws/account/inner": "/nonexistent"}}},
		"git repository":                   {Given: Given{Symlinks: link, Git: &Git{Dir: "ws/account"}}},
	} {
		t.Run(name, func(t *testing.T) {
			if s.Given.Git != nil && git == "" {
				t.Skip("git is not installed")
			}
			home := t.TempDir()
			t.Cleanup(homeguard.SetAccountHome(home))
			protected := filepath.Join(home, ".codex")
			probe := filepath.Join(protected, "probe.json")
			if err := os.MkdirAll(protected, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(probe, []byte("original"), 0o600); err != nil {
				t.Fatal(err)
			}
			stamp := time.Date(2025, 6, 1, 0, 0, 0, 0, time.UTC)
			if err := os.Chtimes(probe, stamp, stamp); err != nil {
				t.Fatal(err)
			}
			links := map[string]string{}
			for rel, target := range s.Given.Symlinks {
				links[rel] = strings.ReplaceAll(target, "${HOME_FAKE}", home)
			}
			s.Given.Symlinks = links
			s.ID = "cli__sh__account_home_link"
			_, err := RunScenario(shRuntime{git: git}, shOptions(t), s)
			var refusal *homeguard.Error
			if !errors.As(err, &refusal) {
				t.Errorf("want a refusal of the account home, got %v", err)
			}
			if raw, err := os.ReadFile(probe); err != nil || string(raw) != "original" {
				t.Errorf("the protected file now holds %q (%v)", raw, err)
			}
			info, err := os.Stat(probe)
			if err != nil {
				t.Fatal(err)
			}
			if info.Mode().Perm() != 0o600 || !info.ModTime().Equal(stamp) {
				t.Errorf("the protected file now has mode %v and mtime %v", info.Mode().Perm(), info.ModTime())
			}
			if dir, err := os.Stat(protected); err != nil || dir.Mode().Perm() != 0o700 {
				t.Errorf("the protected directory: %v, %v", dir, err)
			}
			entries, _ := os.ReadDir(protected)
			if len(entries) != 1 {
				var names []string
				for _, e := range entries {
					names = append(names, e.Name())
				}
				t.Errorf("the protected directory holds %v", names)
			}
		})
	}
}

// CRW-1186 evaluation d1: a stub name is a file name in the case's stubs directory. A name that leads
// out of it (a path with separators, "..") is refused before the installer or the scripted answer
// writes anything, and the destination of the installer is guarded as well, so a link on the way
// (the stubs directory led into the account home) is refused before the program is written.
func TestInstallStubs_aStubNameNeverLeavesTheStubsDirectory(t *testing.T) {
	home := t.TempDir()
	t.Cleanup(homeguard.SetAccountHome(home))
	protected := filepath.Join(home, ".codex")
	if err := os.MkdirAll(protected, 0o700); err != nil {
		t.Fatal(err)
	}
	config := filepath.Join(protected, "config.toml")
	if err := os.WriteFile(config, []byte("original"), 0o600); err != nil {
		t.Fatal(err)
	}
	install := func(c *Case) func(string) error {
		return func(name string) error {
			return os.WriteFile(filepath.Join(c.Root, "stubs", name), []byte("#!/bin/sh\nexit 127\n"), 0o755)
		}
	}
	newCase := func(t *testing.T) *Case {
		c, err := NewCase(t.TempDir(), "CRW_HOME", Given{})
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	for _, name := range []string{"../../../../../../.." + config, "../x", "a/b", "..", ".", "", "sub/../../x"} {
		c := newCase(t)
		err := InstallStubs(c, Given{Stubs: map[string]*Stub{name: {Exit: 127}}}, install(c))
		if err == nil {
			t.Errorf("stub name %q: installed", name)
		}
		if raw, _ := os.ReadFile(config); string(raw) != "original" {
			t.Fatalf("stub name %q: the account's config now holds %q", name, raw)
		}
		if _, err := os.Lstat(filepath.Join(c.Root, "x")); err == nil {
			t.Errorf("stub name %q: a program left the stubs directory", name)
		}
	}
	// the stubs directory itself leads into the account home
	c := newCase(t)
	if err := os.RemoveAll(filepath.Join(c.Root, "stubs")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(protected, filepath.Join(c.Root, "stubs")); err != nil {
		t.Fatal(err)
	}
	err := InstallStubs(c, Given{Stubs: map[string]*Stub{"git": {Exit: 1}}}, install(c))
	var refusal *homeguard.Error
	if !errors.As(err, &refusal) {
		t.Errorf("a stubs directory that leads into the account home: want a refusal, got %v", err)
	}
	if entries, _ := os.ReadDir(protected); len(entries) != 1 {
		t.Errorf("the protected directory holds %d entries", len(entries))
	}
	// an ordinary name still installs
	ok := newCase(t)
	if err := InstallStubs(ok, Given{Stubs: map[string]*Stub{"mytool": {Exit: 2}}}, install(ok)); err != nil {
		t.Errorf("an ordinary stub: %v", err)
	}
	if _, err := os.Stat(filepath.Join(ok.Root, "stubs", "mytool")); err != nil {
		t.Error(err)
	}
}

// CRW-1186 evaluation d2: Given.Symlinks can put ws/.git in the account home (the link's own place is
// safe), and a .git file can name a git directory there. The git directory and the common directory
// git works on are guarded before any git command runs, not only the working directory.
func TestRunScenario_aGitDirectoryInTheAccountHomeIsRefused(t *testing.T) {
	git, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git is not installed")
	}
	for name, given := range map[string]func(home string) Given{
		"a .git link": func(home string) Given {
			return Given{Symlinks: map[string]string{"ws/.git": filepath.Join(home, ".codex", "repo.git")}, Git: &Git{Dir: "ws"}}
		},
		"a .git file": func(home string) Given {
			return Given{Files: map[string]string{"ws/.git": "gitdir: " + filepath.Join(home, ".codex", "repo.git") + "\n"}, Git: &Git{Dir: "ws"}}
		},
		// verify-post d2: a repository git cannot read yet (rev-parse fails) still has its common
		// directory read by git init from the git directory's commondir file.
		"an uninitialized repository whose commondir is in the account home": func(home string) Given {
			return Given{Files: map[string]string{"ws/.git/commondir": filepath.Join(home, ".codex", "repo.git") + "\n"}, Git: &Git{Dir: "ws"}}
		},
		// A relative commondir is taken from the git directory (ws/.git), not from ws: ../acct is ws/acct.
		"a relative commondir taken from the git directory": func(home string) Given {
			return Given{
				Files:    map[string]string{"ws/.git/commondir": "../acct\n"},
				Symlinks: map[string]string{"ws/acct": filepath.Join(home, ".codex", "repo.git")},
				Git:      &Git{Dir: "ws"},
			}
		},
		// The git directory a .git file names (tmp/gd, safe) holds the commondir that leads into the account home.
		"a commondir in the git directory a .git file names": func(home string) Given {
			return Given{
				Files: map[string]string{"ws/.git": "gitdir: ../tmp/gd\n", "tmp/gd/commondir": filepath.Join(home, ".codex", "repo.git") + "\n"},
				Git:   &Git{Dir: "ws"},
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			home := t.TempDir()
			t.Cleanup(homeguard.SetAccountHome(home))
			repo := filepath.Join(home, ".codex", "repo.git")
			if err := os.MkdirAll(repo, 0o700); err != nil {
				t.Fatal(err)
			}
			s := Scenario{ID: "cli__sh__account_home_git", Given: given(home)}
			_, err := RunScenario(shRuntime{git: git}, shOptions(t), s)
			var refusal *homeguard.Error
			if !errors.As(err, &refusal) {
				t.Errorf("want a refusal of the account home, got %v", err)
			}
			if entries, _ := os.ReadDir(repo); len(entries) != 0 {
				t.Errorf("git wrote %d entries into the account's %s", len(entries), repo)
			}
		})
	}
}
