//go:build dev

package laneparity

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/dev/cxccorpus"
	"github.com/thisisjun786/codex-relay-workflow/internal/hookswitch"
)

// A root fired without the switch file is the installation nobody switched: every ported leg ends
// in silence before it reads its payload (CRW-392), so the fixtures and the probe that expect an
// answer differ, and every firing of a ported leg exits 0 with nothing on stdout.
func TestFire_withoutTheSwitchFileThePortedLegsAreSilent(t *testing.T) {
	o := fireFixture(t)
	o.Switch = SwitchOff
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

// The switch the harness writes is the document crw install switch writes, in the Codex home of the
// step, and its undo leaves the tree as the scenario and the hooks made it.
func TestSeed_writesTheSwitchAndTakesItOutAgain(t *testing.T) {
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
		c := &cxccorpus.Case{Root: root}
		undo, err := seedPlan{Switch: SwitchOn}.seed(c, []string{"HOME=" + filepath.Join(root, "home"), "CODEX_HOME=" + codexHome})
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
}

// The switch goes where the hooks of the step look for it: the CODEX_HOME the step's environment
// names, else ~/.codex (a step that unsets CODEX_HOME), made when it is not there, and a directory a
// hook wrote into stays after the undo. A state of off writes nothing, cxc writes cxc.
func TestSeed_putsTheSwitchInTheCodexHomeOfTheStep(t *testing.T) {
	root := t.TempDir()
	home := filepath.Join(root, "home")
	// no CODEX_HOME: ~/.codex under the step's HOME, which does not exist yet
	undo, err := seedPlan{Switch: SwitchCXC}.seed(&cxccorpus.Case{Root: root}, []string{"HOME=" + home})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(hookswitch.Path(filepath.Join(home, ".codex")))
	if err != nil || !strings.Contains(string(raw), `"active":"cxc"`) {
		t.Fatalf("switch %s (%v)", raw, err)
	}
	// a hook wrote below the directory the seed made
	if err := os.WriteFile(filepath.Join(home, ".codex", "crw", "hook-observations"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := undo(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(home, ".codex", "crw", "hook-observations")); err != nil {
		t.Errorf("the undo removed what a hook wrote: %v", err)
	}
	if _, err := os.Lstat(hookswitch.Path(filepath.Join(home, ".codex"))); !os.IsNotExist(err) {
		t.Errorf("the switch file is still there: %v", err)
	}
	// a CODEX_HOME the step names itself, below directories that are not there
	named := filepath.Join(root, "a", "b")
	undo, err = seedPlan{Switch: SwitchOn}.seed(&cxccorpus.Case{Root: root}, []string{"HOME=" + home, "CODEX_HOME=" + named})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(hookswitch.Path(named)); err != nil {
		t.Fatal(err)
	}
	if err := undo(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(root, "a")); !os.IsNotExist(err) {
		t.Errorf("the undo left a directory it made: %v", err)
	}
	// off writes nothing, and a case that names no home at all cannot be seeded
	undo, err = seedPlan{Switch: SwitchOff}.seed(&cxccorpus.Case{Root: root}, []string{"HOME=" + home, "CODEX_HOME=" + named})
	if err != nil || undo() != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(named); !os.IsNotExist(err) {
		t.Errorf("off wrote into %s: %v", named, err)
	}
	if _, err := (seedPlan{Switch: SwitchOn}).seed(&cxccorpus.Case{Root: root}, nil); err == nil {
		t.Error("a step without CODEX_HOME and HOME was seeded")
	}
	// nothing is written outside the case root, whatever the step names
	outside := filepath.Join(t.TempDir(), "codex")
	if _, err := (seedPlan{Switch: SwitchOn}).seed(&cxccorpus.Case{Root: root}, []string{"CODEX_HOME=" + outside}); err == nil {
		t.Error("a Codex home outside the case root was seeded")
	}
	if _, err := os.Lstat(outside); !os.IsNotExist(err) {
		t.Errorf("something was written to %s: %v", outside, err)
	}
	if _, err := (seedPlan{Switch: SwitchOn, Runtime: true, CRW: "/x/crw"}).seed(&cxccorpus.Case{Root: root}, []string{"HOME=" + filepath.Dir(root)}); err == nil {
		t.Error("a HOME outside the case root got a runtime link")
	}
}

// The shipped plugin starts the installed runtime ("$HOME/.local/share/crw-runtime/current/bin/crw"):
// the harness links it to the build under test in the HOME of the step, as the installer's pointer
// would, and takes the link and the directories it made out again.
func TestSeed_linksTheRuntimeForTheCommandsThatStartIt(t *testing.T) {
	root := t.TempDir()
	home := filepath.Join(root, "home")
	if err := os.MkdirAll(home, 0o755); err != nil {
		t.Fatal(err)
	}
	crw := filepath.Join(root, "build", "crw")
	declared := map[string]string{"a": `"$HOME/` + runtimeBin + `" hook session-start --leg a`}
	if p := newSeedPlan(SwitchOn, crw, map[string]string{"a": `"` + crw + `" hook x`}); p.Runtime {
		t.Error("a command that starts the build by path asks for the runtime link")
	}
	plan := newSeedPlan(SwitchOn, crw, declared)
	if !plan.Runtime {
		t.Fatal("the shipped command form asks for no runtime link")
	}
	undo, err := plan.seed(&cxccorpus.Case{Root: root}, []string{"HOME=" + home, "CODEX_HOME=" + filepath.Join(home, "codex")})
	if err != nil {
		t.Fatal(err)
	}
	if target, err := os.Readlink(filepath.Join(home, runtimeBin)); err != nil || target != crw {
		t.Errorf("runtime link %q (%v)", target, err)
	}
	if got, ok := CommandExecutable(declared["a"], crw); !ok || got != crw {
		t.Errorf("the executable of the shipped command is %q %v", got, ok)
	}
	if err := undo(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(home, ".local")); !os.IsNotExist(err) {
		t.Errorf("the undo left the directories it made: %v", err)
	}
}

// A step that names a Codex home of its own, or unsets CODEX_HOME, runs its hooks against the switch
// in that home: the fixtures whose steps do are fired with the switch at crw and match their
// expectation (before CRW-1082 the switch was written into the case's CODEX_HOME only, so the
// permission-request and the managed-worktree fixtures below ran against no switch and were silent).
func TestFire_theSwitchIsInTheCodexHomeOfEveryStep(t *testing.T) {
	o := fireFixture(t)
	o.Only = regexpOf(`^hook__(permission-request-allowing-agent-thread__(home_cwd_default_config|codex_config_must_be_full_access)|session-start-detecting-managed-worktree__unmanaged_and_root_are_silent)$`)
	rep, err := Fire(o)
	if err != nil {
		t.Fatal(err)
	}
	if !rep.OK {
		t.Fatalf("report not ok: fixtures %+v receipts %q", failed(rep), rep.ReceiptProblems)
	}
	ran := 0
	for _, f := range rep.Fixtures {
		if f.Run && f.OK {
			ran++
		}
	}
	if ran != 3 {
		t.Errorf("%d of the 3 fixtures ran and matched", ran)
	}
}

// At cxc the ported legs are as silent as with no switch file.
func TestFire_atCxcThePortedLegsAreSilent(t *testing.T) {
	o := fireFixture(t)
	o.Switch = SwitchCXC
	rep, err := Fire(o)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Switch.Absent || rep.Switch.Active != "cxc" {
		t.Errorf("switch report %+v", rep.Switch)
	}
	if rep.OK {
		t.Fatal("a root fired at cxc passed: the ported legs answered")
	}
	silent, ported := StdoutDigest(""), 0
	for _, r := range rep.Receipts {
		if r.Leg == CompletionLeg {
			continue
		}
		ported++
		if r.Exit != 0 || r.StdoutSHA256 != silent {
			t.Errorf("%s step %d (%s): exit %d, stdout sha256 %s: a ported leg answered at cxc", r.Fixture, r.Step, r.Leg, r.Exit, r.StdoutSHA256)
		}
	}
	if ported < 20 {
		t.Errorf("%d firings of ported legs", ported)
	}
}

// The plugin that ships is fired as it is: its declarations start "$HOME/.local/share/crw-runtime/
// current/bin/crw", the harness links that path to the build under test in every step's HOME, and the
// receipts name that build, where a generated root names it in the command. Nothing is left linked.
func TestFire_theShippedPluginFiresThroughItsOwnDeclarations(t *testing.T) {
	o := fireFixture(t)
	o.Plugin = filepath.Join(o.Root, "plugins", "crw")
	rep, err := Fire(o)
	if err != nil {
		t.Fatal(err)
	}
	if !rep.OK {
		t.Fatalf("report not ok: receipts %q fixtures %+v probes %+v", rep.ReceiptProblems, failed(rep), rep.Probes)
	}
	if len(rep.Receipts) < 20 {
		t.Fatalf("%d receipts", len(rep.Receipts))
	}
	for _, r := range rep.Receipts {
		if r.Binary != rep.Binary || !strings.Contains(r.Command, "$HOME/"+runtimeBin) {
			t.Fatalf("receipt %+v: not the shipped command on the build under test", r)
		}
	}
	if left, _ := os.ReadDir(o.Scratch); len(left) != 0 {
		t.Errorf("%d entries left in the scratch directory", len(left))
	}
}
