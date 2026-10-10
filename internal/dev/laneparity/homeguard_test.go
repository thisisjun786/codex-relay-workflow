//go:build dev

package laneparity

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/dev/cxccorpus"
	"github.com/thisisjun786/codex-relay-workflow/internal/dev/homeguard"
	"github.com/thisisjun786/codex-relay-workflow/internal/hookswitch"
)

// CRW-1186: a laneparity run wrote <account home>/.codex/crw/switch.json (active=crw, by=laneparity).
// These tests name a fake account home (homeguard.SetAccountHome) and point HOME and CODEX_HOME at it,
// the way a run or a test that holds the real account's variables does, and show that no writer
// creates anything below it.

// fakeAccount is a directory standing for the account's real home, and the lookup that says so.
func fakeAccount(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Cleanup(homeguard.SetAccountHome(home))
	return home
}

// untouched fails when the fake account home holds anything below the protected directories.
func untouched(t *testing.T, home string) {
	t.Helper()
	for _, rel := range []string{".codex", ".crw", ".local/share/crw-runtime"} {
		if _, err := os.Lstat(filepath.Join(home, rel)); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s exists in the account home (%v)", rel, err)
		}
	}
}

func wantRefusal(t *testing.T, err error) {
	t.Helper()
	var refusal *homeguard.Error
	if !errors.As(err, &refusal) {
		t.Fatalf("want a refusal of the account home, got %v", err)
	}
	if !strings.Contains(err.Error(), "account's real home") {
		t.Errorf("the error does not say why: %v", err)
	}
}

func TestSeed_refusesTheAccountHomeWhateverNamesIt(t *testing.T) {
	linkTo := func(t *testing.T, target string) string {
		link := filepath.Join(t.TempDir(), "alias")
		if err := os.Symlink(target, link); err != nil {
			t.Fatal(err)
		}
		return link
	}
	for _, tc := range []struct {
		name string
		plan seedPlan
		// root and env are built from the account home
		root func(t *testing.T, home string) string
		env  func(root, home string) []string
	}{
		{"the account home is the case root, HOME and CODEX_HOME name it", seedPlan{Switch: SwitchOn},
			func(_ *testing.T, home string) string { return home },
			func(_, home string) []string {
				return []string{"HOME=" + home, "CODEX_HOME=" + filepath.Join(home, ".codex")}
			}},
		{"HOME alone: CODEX_HOME falls back to HOME/.codex", seedPlan{Switch: SwitchCXC},
			func(_ *testing.T, home string) string { return home },
			func(_, home string) []string { return []string{"HOME=" + home} }},
		{"the case root is a link to the account home", seedPlan{Switch: SwitchOn},
			func(t *testing.T, home string) string { return linkTo(t, home) },
			func(root, _ string) []string {
				return []string{"HOME=" + root, "CODEX_HOME=" + filepath.Join(root, ".codex")}
			}},
		{"the runtime link goes below HOME even with the switch off", seedPlan{Switch: SwitchOff, Runtime: true, CRW: "/opt/crw"},
			func(_ *testing.T, home string) string { return home },
			func(_, home string) []string { return []string{"HOME=" + home} }},
		{"a case root below the account's .codex", seedPlan{Switch: SwitchOn},
			func(t *testing.T, home string) string {
				root := filepath.Join(home, ".codex", "scratch")
				if err := os.MkdirAll(root, 0o755); err != nil {
					t.Fatal(err)
				}
				return root
			},
			func(root, _ string) []string {
				return []string{"HOME=" + filepath.Join(root, "home"), "CODEX_HOME=" + filepath.Join(root, "codex")}
			}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := fakeAccount(t)
			root := tc.root(t, home)
			undo, err := tc.plan.seed(&cxccorpus.Case{Root: root}, tc.env(root, home))
			if err == nil {
				_ = undo()
			}
			wantRefusal(t, err)
			for _, rel := range []string{".codex/crw/switch.json", ".local/share/crw-runtime", ".codex/scratch/codex", ".codex/scratch/home"} {
				if _, err := os.Lstat(filepath.Join(home, rel)); !errors.Is(err, os.ErrNotExist) {
					t.Errorf("%s was created in the account home (%v)", rel, err)
				}
			}
		})
	}
}

func TestWriteSwitch_refusesTheAccountHome(t *testing.T) {
	home := fakeAccount(t)
	dir := filepath.Join(home, ".codex", "crw")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	wantRefusal(t, writeSwitch(filepath.Join(dir, "switch.json"), hookswitch.State{Active: hookswitch.CRW, By: SwitchBy}))
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Errorf("the account's crw directory holds %d entries", len(entries))
	}
}

func TestGeneratePluginRoot_refusesTheAccountHome(t *testing.T) {
	home := fakeAccount(t)
	root := repoRoot(t)
	err := GeneratePluginRoot(filepath.Join(home, ".codex", "plugins", "crw"), filepath.Join(root, "plugins", "crw"), "/opt/crw", nil)
	wantRefusal(t, err)
	untouched(t, home)
}

func TestScratch_aRunNeverUsesTheAccountHomeAsItsScratch(t *testing.T) {
	home := fakeAccount(t)
	scratch := filepath.Join(home, ".crw", "scratch")
	root := repoRoot(t)
	plugin := filepath.Join(root, "plugins", "crw")
	_, err := RealHost(RealHostOptions{Root: root, CRW: os.Args[0], Plugin: plugin, Codex: filepath.Join(t.TempDir(), "no-codex"), Scratch: scratch})
	wantRefusal(t, err)
	untouched(t, home)

	o := fireFixture(t)
	o.Scratch = scratch
	_, err = Fire(o)
	wantRefusal(t, err)
	untouched(t, home)

	// a default scratch (TMPDIR) that names the account's home is refused as well
	o = fireFixture(t)
	o.Scratch = ""
	tmp := filepath.Join(home, ".codex", "tmp")
	if err := os.MkdirAll(tmp, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TMPDIR", tmp)
	_, err = Fire(o)
	wantRefusal(t, err)
	if entries, _ := os.ReadDir(tmp); len(entries) != 0 {
		t.Errorf("a scratch directory was made in the account's .codex: %d entries", len(entries))
	}
}

func TestRun_refusesAnOutputOrScratchInTheAccountHome(t *testing.T) {
	home := fakeAccount(t)
	for _, tc := range []struct {
		command string
		args    []string
	}{
		{"plugin-root", []string{"--crw", "/opt/crw", "--out", filepath.Join(home, ".codex", "root")}},
		{"fire", []string{"--crw", "/opt/crw", "--scratch", filepath.Join(home, ".codex", "scratch")}},
		{"fire", []string{"--crw", "/opt/crw", "--json", filepath.Join(home, ".crw", "report.json")}},
		{"all", []string{"--crw", "/opt/crw", "--scratch", filepath.Join(home, ".local", "share", "crw-runtime", "scratch")}},
	} {
		var stdout, stderr bytes.Buffer
		code := Run(append([]string{tc.command}, tc.args...), &stdout, &stderr)
		if code == 0 || !strings.Contains(stderr.String(), "account's real home") {
			t.Errorf("%s %v: exit %d, stderr %q", tc.command, tc.args, code, stderr.String())
		}
		untouched(t, home)
	}
}
