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
