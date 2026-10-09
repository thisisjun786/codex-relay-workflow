//go:build dev

package laneparity

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/dev/cxccorpus"
	"github.com/thisisjun786/codex-relay-workflow/internal/hookswitch"
)

// A root fired without the switch file is the installation nobody switched: every ported leg ends
// in silence before it reads its payload (CRW-392), so the fixtures and the probe that expect an
// answer differ, and every firing of a ported leg exits 0 with nothing on stdout.
func TestFire_withoutTheSwitchFileThePortedLegsAreSilent(t *testing.T) {
	o := fireFixture(t)
	o.NoSwitch = true
	rep, err := Fire(o)
	if err != nil {
		t.Fatal(err)
	}
	if !rep.Switch.Absent || rep.Switch.Active != "" || rep.Switch.File != hookswitch.Path("<CODEX_HOME>") {
		t.Errorf("switch report %+v", rep.Switch)
	}
	if rep.OK {
		t.Fatal("a root fired without the switch passed: the ported legs answered")
	}
	silent, ported := StdoutDigest(""), 0
	legs := map[string]bool{}
	for _, r := range rep.Receipts {
		if r.Leg == CompletionLeg { // the completion Stop is not behind the switch (crw hook --plugin-launch)
			continue
		}
		ported++
		legs[r.Leg] = true
		if r.Exit != 0 || r.StdoutSHA256 != silent {
			t.Errorf("%s step %d (%s): exit %d, stdout sha256 %s: a ported leg answered without the switch", r.Fixture, r.Step, r.Leg, r.Exit, r.StdoutSHA256)
		}
	}
	for _, leg := range []string{"session-start-announcing-map-affordance", "pre-tool-use-guarding-managed-worktree-deletion", "subagent-stop-verifying-evidence", GitHubPostLeg} {
		if !legs[leg] {
			t.Errorf("no firing of %s was recorded", leg)
		}
	}
	if ported < 20 {
		t.Errorf("%d firings of ported legs", ported)
	}
	for _, p := range rep.Probes {
		if p.ID == "github-post-denies-inline-body" && p.OK {
			t.Errorf("the GitHub post guard denied without the switch: %+v", p)
		}
	}
}

// The switch the harness writes is the document crw install switch writes, in the case's
// CODEX_HOME, and its undo leaves the tree as the scenario and the hooks made it.
func TestSeedSwitch_writesTheSwitchAndTakesItOutAgain(t *testing.T) {
	for _, existing := range []bool{false, true} {
		root := t.TempDir()
		codexHome := filepath.Join(root, "codex")
		if err := os.MkdirAll(codexHome, 0o755); err != nil {
			t.Fatal(err)
		}
		if existing {
			if err := os.Mkdir(filepath.Join(codexHome, "crw"), 0o755); err != nil {
				t.Fatal(err)
			}
		}
		c := &cxccorpus.Case{Root: root, Env: []string{"HOME=" + filepath.Join(root, "home"), "CODEX_HOME=" + codexHome}}
		undo, err := seedSwitch(c)
		if err != nil {
			t.Fatal(err)
		}
		raw, err := os.ReadFile(hookswitch.Path(codexHome))
		if err != nil {
			t.Fatal(err)
		}
		var s hookswitch.State
		if err := json.Unmarshal(raw, &s); err != nil || s.Active != hookswitch.CRW || s.By != SwitchBy || s.ChangedAt == "" {
			t.Fatalf("switch %s (%v)", raw, err)
		}
		if r := hookswitch.Read(func(k string) (string, bool) { return map[string]string{"CODEX_HOME": codexHome}[k], k == "CODEX_HOME" }); !r.On || r.Problem != "" {
			t.Errorf("the hooks read the seeded switch as %+v", r)
		}
		entries, _ := os.ReadDir(filepath.Join(codexHome, "crw"))
		if len(entries) != 1 {
			t.Errorf("existing %v: %d entries beside the switch (a temporary file left?)", existing, len(entries))
		}
		info, err := os.Stat(filepath.Join(codexHome, "crw"))
		if err != nil {
			t.Fatal(err)
		}
		if want := map[bool]os.FileMode{false: 0o700, true: 0o755}[existing]; info.Mode().Perm() != want {
			t.Errorf("existing %v: crw directory mode %v, want %v", existing, info.Mode().Perm(), want)
		}
		if err := undo(); err != nil {
			t.Fatal(err)
		}
		_, err = os.Stat(filepath.Join(codexHome, "crw"))
		if existing && err != nil {
			t.Errorf("the undo removed a directory the case had: %v", err)
		}
		if !existing && !os.IsNotExist(err) {
			t.Errorf("the undo left the directory it made: %v", err)
		}
		if _, err := os.Lstat(hookswitch.Path(codexHome)); !os.IsNotExist(err) {
			t.Errorf("the switch file is still there: %v", err)
		}
	}
	// A directory the hooks wrote into stays.
	root := t.TempDir()
	codexHome := filepath.Join(root, "codex")
	undo, err := seedSwitch(&cxccorpus.Case{Root: root, Env: []string{"CODEX_HOME=" + codexHome}})
	if err == nil {
		t.Fatal("a missing CODEX_HOME directory was seeded")
	}
	_ = undo
	if err := os.MkdirAll(codexHome, 0o755); err != nil {
		t.Fatal(err)
	}
	if undo, err = seedSwitch(&cxccorpus.Case{Root: root, Env: []string{"CODEX_HOME=" + codexHome}}); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(codexHome, "crw", "hook-observations"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := undo(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(codexHome, "crw", "hook-observations")); err != nil {
		t.Errorf("the undo removed what a hook wrote: %v", err)
	}
	if _, err := seedSwitch(&cxccorpus.Case{Root: root}); err == nil {
		t.Error("a case without CODEX_HOME was seeded")
	}
}
