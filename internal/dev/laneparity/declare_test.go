//go:build dev

package laneparity

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/contracttest"
	"github.com/thisisjun786/codex-relay-workflow/internal/dev/cxccorpus"
)

func repoRoot(t *testing.T) string {
	t.Helper()
	root, err := contracttest.Root()
	if err != nil {
		t.Fatal(err)
	}
	return root
}

func TestExpectedLegs_areK1ThenTheTwoOwnRegistrations(t *testing.T) {
	legs, err := ExpectedLegs(repoRoot(t))
	if err != nil {
		t.Fatal(err)
	}
	if len(legs) != 34 {
		t.Fatalf("%d legs, want 32 of K1 and 2 own", len(legs))
	}
	for i, l := range legs {
		if !strings.HasPrefix(l.Status, "(crw) ") || strings.Contains(l.Status, "codexclaw") {
			t.Errorf("%s: status %q is not renamed by R24", l.Leg, l.Status)
		}
		if !strings.HasPrefix(l.File, "wiring/hooks/") {
			t.Errorf("%s: file %q is not under wiring/hooks", l.Leg, l.File)
		}
		if l.Own != (i >= 32) {
			t.Errorf("%s: Own = %v at index %d", l.Leg, l.Own, i)
		}
	}
	if legs[32].Leg != CompletionLeg || legs[33].Leg != GitHubPostLeg {
		t.Errorf("own legs are %s and %s", legs[32].Leg, legs[33].Leg)
	}
	if g := cxccorpus.GitHubPostGuard; legs[33].Leg != g.Leg || legs[33].Event != g.Event || legs[33].Matcher != g.Matcher || legs[33].Timeout != g.Timeout || legs[33].Status != g.Status {
		t.Errorf("the GitHub post leg %+v is not the declaration the shipped plugin generates %+v", legs[33], g)
	}
}

func TestRoute(t *testing.T) {
	for _, c := range []struct {
		command, event, leg string
		ok                  bool
	}{
		{`"/x/crw" hook session-start --leg session-start-bootstrapping-pabcd-state`, "session-start", "session-start-bootstrapping-pabcd-state", true},
		{`"$HOME/.local/share/crw-runtime/current/bin/crw" hook post-compact --leg=a.post-compact; exit 0`, "post-compact", "a.post-compact", true},
		{`"$HOME/.local/share/crw-runtime/current/bin/crw" hook --plugin-launch; exit 0`, "stop", CompletionLeg, true},
		{`node "${PLUGIN_ROOT}/components/pabcd-state/dist/cli.js" hook session-start`, "", "", false},
		{`exit 0`, "", "", false},
		{`echo hook stop --legacy x`, "", "", false},
		{`crw hook session-start --leg x`, "session-start", "x", true},
		{`"$CRW_BIN" hook stop --leg x`, "stop", "x", true},
		{`${CRW_BIN} hook stop --leg x;`, "stop", "x", true},
		// The hook syntax must be what the shell runs, not text it skips or passes to something else.
		{`"/x/crw" --version >/dev/null # "/x/crw" hook session-start --leg x`, "", "", false},
		{`/bin/true # "/x/crw" hook session-start --leg x`, "", "", false},
		{`echo "/x/crw" hook session-start --leg x`, "", "", false},
		{`/bin/echo hook session-start --leg x`, "", "", false},
		{`false && "/x/crw" hook session-start --leg x`, "", "", false},
		{`if false; then "/x/crw" hook session-start --leg x; fi`, "", "", false},
		{`"/x/crw" hook session-start --leg x || true`, "", "", false},
		{`"/x/crw" hook session-start --leg x > /dev/null`, "", "", false},
		{`"/x/crw" hook session-start --leg x; /bin/sleep 1`, "", "", false},
		{`"/x/crw" hook session-start --leg x extra`, "", "", false},
		{`"/x/crw" hook --plugin-launch; exit 3`, "", "", false},
		{`"/x/crw" hook session-start --leg "x"`, "", "", false},
		{`"$(id)/crw" hook session-start --leg x`, "", "", false},
		{"`id`/crw hook session-start --leg x", "", "", false},
		{`relative/crw hook session-start --leg x`, "", "", false},
		{`'/x/crw' hook session-start --leg x`, "", "", false},
		{"\"/x/crw\" hook session-start --leg x\n/bin/true", "", "", false},
	} {
		event, leg, ok := Route(c.command)
		if event != c.event || leg != c.leg || ok != c.ok {
			t.Errorf("Route(%q) = %q %q %v, want %q %q %v", c.command, event, leg, ok, c.event, c.leg, c.ok)
		}
	}
}

func generated(t *testing.T) (string, []Leg) {
	t.Helper()
	root := repoRoot(t)
	legs, err := ExpectedLegs(root)
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(t.TempDir(), "crw")
	if err := GeneratePluginRoot(dir, filepath.Join(root, "plugins", "crw"), "/opt/crw-under-test/crw", legs); err != nil {
		t.Fatal(err)
	}
	return dir, legs
}

func TestGeneratePluginRoot_declaresEveryLegOnceAndKeepsTheSkills(t *testing.T) {
	dir, legs := generated(t)
	manifest, got, err := ReadRegistered(dir)
	if err != nil {
		t.Fatal(err)
	}
	if rep := CheckRegistration(manifest, got, legs); !rep.OK {
		t.Fatalf("a generated root does not register as required: %+v", rep)
	}
	if len(got) != 34 {
		t.Errorf("%d registrations, want 34", len(got))
	}
	for _, r := range got {
		if !strings.HasPrefix(r.Command, `"/opt/crw-under-test/crw" hook `) {
			t.Errorf("%s: command %q does not start the build under test", r.Leg, r.Command)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "skills", "crw-run", "SKILL.md")); err != nil {
		t.Errorf("the skills are not reachable through the root: %v", err)
	}
	if _, err := PluginDigest(dir); err != nil {
		t.Error(err)
	}
}

func TestGeneratePluginRoot_refusesACrwPathTheShellWouldReadDifferently(t *testing.T) {
	legs, _ := ExpectedLegs(repoRoot(t))
	for _, bad := range []string{"relative/crw", `/x/"crw`, "/x/$HOME/crw", "/x/`id`/crw"} {
		if err := GeneratePluginRoot(t.TempDir()+"/r", filepath.Join(repoRoot(t), "plugins", "crw"), bad, legs); err == nil {
			t.Errorf("%q was accepted", bad)
		}
	}
}

func TestPluginDigest_differsWhenADeclarationChanges(t *testing.T) {
	a, _ := generated(t)
	b, _ := generated(t)
	da, _ := PluginDigest(a)
	db, _ := PluginDigest(b)
	if da != db {
		t.Fatal("two generated roots of one build differ")
	}
	path := filepath.Join(b, "wiring", "hooks", "stop-recording-completion.json")
	raw, _ := os.ReadFile(path)
	if err := os.WriteFile(path, []byte(strings.Replace(string(raw), `"timeout": 10`, `"timeout": 11`, 1)), 0o644); err != nil {
		t.Fatal(err)
	}
	if dc, _ := PluginDigest(b); dc == da {
		t.Error("the digest ignores a changed timeout")
	}
}

// A hook file the manifest lists anywhere in the root is a declaration: the digest that keys report
// reuse holds it, so changing a matcher or timeout there is another plugin root.
func TestPluginDigest_holdsADeclaredHookFileOutsideWiringHooks(t *testing.T) {
	dir, _ := generated(t)
	manifestPath := filepath.Join(dir, ".codex-plugin", "plugin.json")
	raw, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	var manifest map[string]any
	if err := json.Unmarshal(raw, &manifest); err != nil {
		t.Fatal(err)
	}
	manifest["hooks"] = append(manifest["hooks"].([]any), "./h.json")
	out, _ := json.Marshal(manifest)
	if err := os.WriteFile(manifestPath, out, 0o644); err != nil {
		t.Fatal(err)
	}
	external := filepath.Join(dir, "h.json")
	write := func(timeout string) {
		t.Helper()
		body := `{"hooks":{"Stop":[{"matcher":"","hooks":[{"type":"command","command":"/bin/true","timeout":` + timeout + `}]}]}}`
		if err := os.WriteFile(external, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("10")
	a, err := PluginDigest(dir)
	if err != nil {
		t.Fatal(err)
	}
	write("11")
	if b, _ := PluginDigest(dir); b == a {
		t.Error("the digest ignores a declared hook file outside wiring/hooks")
	}
}

// A relative source checkout (--repo .) must not become the target of links in another directory.
func TestGeneratePluginRoot_aRelativeSourceKeepsTheSkills(t *testing.T) {
	root := repoRoot(t)
	legs, err := ExpectedLegs(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Chdir(root)
	dest := filepath.Join(t.TempDir(), "crw")
	if err := GeneratePluginRoot(dest, filepath.Join("plugins", "crw"), "/opt/crw-under-test/crw", legs); err != nil {
		t.Fatal(err)
	}
	for _, rel := range []string{filepath.Join("skills", "crw-run", "SKILL.md"), "LICENSE"} {
		if _, err := os.Stat(filepath.Join(dest, rel)); err != nil {
			t.Errorf("%s is not reachable through a root generated from a relative source: %v", rel, err)
		}
	}
}

// The same through the command line: --repo . writes a root whose links resolve.
func TestRun_pluginRootWithARelativeRepo(t *testing.T) {
	root := repoRoot(t)
	t.Chdir(root)
	dest := filepath.Join(t.TempDir(), "crw")
	var out, errs bytes.Buffer
	if code := Run([]string{"plugin-root", "--repo", ".", "--crw", "/opt/crw-under-test/crw", "--out", dest}, &out, &errs); code != 0 {
		t.Fatalf("exit %d: %s %s", code, out.String(), errs.String())
	}
	if _, err := os.Stat(filepath.Join(dest, "skills", "crw-run", "SKILL.md")); err != nil {
		t.Errorf("the generated skills link does not resolve: %v", err)
	}
}
