package doctor

// CRW-1152 verification round 2: the harness's bounded config.toml read stays on the harness path,
// a hook event outside the ten supported names is a hooks finding, and a document whose syntax
// fails before any depth breach keeps the reader's own error.

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

// harnessRound2TrustPlugin is a plugin with one Stop hook and its install key in a config.toml
// padded past harnessReadLimit by a valid TOML comment.
func harnessRound2TrustPlugin(t *testing.T) (root, codexHome string) {
	t.Helper()
	root = t.TempDir()
	harnessBoundedWrite(t, root, ".codex-plugin/plugin.json", `{"name":"crw","hooks":["./hooks/a.json"]}`)
	harnessBoundedWrite(t, root, "hooks/a.json", `{"hooks":{"Stop":[{"hooks":[{"type":"command","command":"echo ok"}]}]}}`)
	codexHome = t.TempDir()
	harnessBoundedWrite(t, codexHome, "config.toml", "# "+strings.Repeat("x", harnessReadLimit)+"\n"+`[plugins."crw@local"]`+"\n"+"enabled = true\n")
	return root, codexHome
}

// The exported readers the retrust command shares keep their unbounded read: a large, valid
// config.toml is diagnosed and its keys are listed, as before CRW-1152.
func TestHarnessRound2SharedConfigReadersAreUnbounded(t *testing.T) {
	root, codexHome := harnessRound2TrustPlugin(t)
	keys, err := ReadInstalledPluginKeys(codexHome, "crw")
	if err != nil || len(keys) != 1 || keys[0] != "crw@local" {
		t.Fatalf("ReadInstalledPluginKeys = %v, %v; want [crw@local]", keys, err)
	}
	results, err := DiagnoseHookTrust(codexHome, root, "crw@local")
	if err != nil || len(results) != 1 || results[0].Status != "untrusted" {
		t.Fatalf("DiagnoseHookTrust = %+v, %v; want one untrusted result", results, err)
	}
}

// The harness's hook-trust check reads the same config.toml within the bound and reports it.
func TestHarnessRound2HarnessConfigReadIsBounded(t *testing.T) {
	root, codexHome := harnessRound2TrustPlugin(t)
	check := harnessBoundedWithin(t, "the hook-trust check", func() HarnessCheck {
		return HarnessHookTrustCheck(root, HarnessOptions{CodexHome: &codexHome}, harnessRunEnv(nil))
	})
	if check.Severity != HarnessFail || !strings.Contains(check.Evidence, errHarnessTooLarge.Error()) {
		t.Fatalf("check = %+v, want a FAIL naming the read limit", check)
	}
	t.Run("fifo", func(t *testing.T) {
		home := t.TempDir()
		harnessBoundedFIFO(t, filepath.Join(home, "config.toml"))
		check := harnessBoundedWithin(t, "the hook-trust check", func() HarnessCheck {
			return HarnessHookTrustCheck(root, HarnessOptions{CodexHome: &home}, harnessRunEnv(nil))
		})
		if check.Severity != HarnessFail || !strings.Contains(check.Evidence, errNotRegular.Error()) {
			t.Fatalf("check = %+v, want a FAIL naming the file that is not regular", check)
		}
	})
}

// Every event outside the ten supported names is a hooks finding; the supported ones are not.
func TestHarnessRound2UnsupportedHookEventsAreFindings(t *testing.T) {
	root := t.TempDir()
	harnessBoundedWrite(t, root, ".codex-plugin/plugin.json", `{"name":"x","hooks":["./hooks/a.json"]}`)
	harnessBoundedWrite(t, root, "dist/ok.js", "")
	harnessBoundedWrite(t, root, "hooks/a.json", `{"hooks":{"DefinitelyNotAnEvent":[],"stop":[],"Stop":[{"hooks":[{"type":"command","command":"node dist/ok.js"}]}],"PreToolUse":[]}}`)
	issues, err := ValidateManifestTargets(root)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{"hook event is not supported: DefinitelyNotAnEvent": true, "hook event is not supported: stop": true}
	for _, issue := range issues {
		if !want[issue.Message] {
			t.Fatalf("unexpected issue %+v (all: %+v)", issue, issues)
		}
		delete(want, issue.Message)
	}
	if len(want) != 0 {
		t.Fatalf("missing findings %v (issues: %+v)", want, issues)
	}
	checks := HarnessManifestTargetChecks(root)
	hooks, ok := harnessBoundedCheck(checks, "hooks")
	if !ok || hooks.Severity != HarnessFail || !strings.Contains(hooks.Evidence, "DefinitelyNotAnEvent") {
		t.Fatalf("hooks check = %+v (all: %+v), want a FAIL naming the event", hooks, checks)
	}
}

// A syntax error before any container passes the depth limit keeps the reader's error; a
// well-formed document past the limit is errHarnessTooDeep.
func TestHarnessRound2SyntaxErrorBeforeDepthIsNotTooDeep(t *testing.T) {
	deep := strings.Repeat("[", 10001) + strings.Repeat("]", 10001)
	if _, err := harnessInstallParseJSON([]byte(`{"x":?,"y":` + deep + `}`)); err == nil || errors.Is(err, errHarnessTooDeep) {
		t.Fatalf("malformed then deep: err = %v, want the reader's syntax error", err)
	}
	if _, err := harnessInstallParseJSON([]byte(deep)); !errors.Is(err, errHarnessTooDeep) {
		t.Fatalf("deep: err = %v, want errHarnessTooDeep", err)
	}
	if _, err := harnessInstallParseJSON([]byte(`{"y":` + deep + `}`)); !errors.Is(err, errHarnessTooDeep) {
		t.Fatalf("deep member: err = %v, want errHarnessTooDeep", err)
	}
	if _, err := harnessInstallParseJSON([]byte(`{"x":?}`)); err == nil || errors.Is(err, errHarnessTooDeep) {
		t.Fatalf("malformed: err = %v, want the reader's syntax error", err)
	}
}
