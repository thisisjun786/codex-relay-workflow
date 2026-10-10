package doctor

// CRW-1152 verification round 1: an unsearchable parent is not an absent directory, one kind of
// target document that cannot be read never hides the other kind's result, and a report field that
// cannot be read does not take the report with it.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestHarnessRound1UnsearchableParentIsNotCleanState(t *testing.T) {
	project := t.TempDir()
	harnessBoundedWrite(t, project, ".crw/sessions/s.json", "{}")
	harnessBoundedLocked(t, filepath.Join(project, ".crw"))
	check := harnessBoundedWithin(t, "the pabcd check", func() HarnessCheck { return HarnessPabcdCheck(project) })
	if check.Severity != HarnessWarn || !strings.Contains(check.Evidence, "EACCES") || strings.Contains(check.Evidence, "clean state") {
		t.Fatalf("check = %+v, want a skipped WARN naming EACCES, not a clean-state PASS", check)
	}
}

func TestHarnessRound1UnsearchableParentOfSkillsAndAgents(t *testing.T) {
	root := t.TempDir()
	harnessBoundedWrite(t, root, "skills/x/SKILL.md", "x")
	harnessBoundedWrite(t, root, "agents/a.toml", "x")
	for _, name := range []string{"skills", "agents"} {
		harnessBoundedLocked(t, filepath.Join(root, name))
	}
	// Locking the directory itself makes its read fail; the parent case is the plugin root.
	for _, check := range []HarnessCheck{harnessRunSkillsCheck(root), harnessRunAgentsCheck(root)} {
		if check.Severity != HarnessWarn || !strings.Contains(check.Evidence, "EACCES") {
			t.Fatalf("check = %+v, want a WARN naming EACCES", check)
		}
	}
	parent := t.TempDir()
	child := filepath.Join(parent, "plugin")
	harnessBoundedWrite(t, child, "skills/x/SKILL.md", "x")
	harnessBoundedLocked(t, child)
	for _, check := range []HarnessCheck{harnessRunSkillsCheck(child), harnessRunAgentsCheck(child)} {
		if check.Severity != HarnessWarn || !strings.Contains(check.Evidence, "EACCES") || strings.Contains(check.Evidence, "no skills/ directory") || strings.Contains(check.Evidence, "no agents/ directory") {
			t.Fatalf("check = %+v, want a skipped WARN naming EACCES, not an absent directory", check)
		}
	}
}

func harnessRound1Targets(t *testing.T, manifest string, fifos ...string) (HarnessCheck, HarnessCheck) {
	t.Helper()
	root := t.TempDir()
	harnessBoundedWrite(t, root, ".codex-plugin/plugin.json", manifest)
	for _, fifo := range fifos {
		harnessBoundedFIFO(t, filepath.Join(root, fifo))
	}
	checks := harnessBoundedWithin(t, "the target checks", func() []HarnessCheck { return HarnessManifestTargetChecks(root) })
	hooks, okH := harnessBoundedCheck(checks, "hooks")
	mcp, okM := harnessBoundedCheck(checks, "mcp-targets")
	if !okH || !okM {
		t.Fatalf("checks = %+v, want a hooks and an mcp-targets check", checks)
	}
	return hooks, mcp
}

func TestHarnessRound1TargetKindsAreIndependent(t *testing.T) {
	t.Run("hooks shape error survives an unreadable mcp file", func(t *testing.T) {
		hooks, mcp := harnessRound1Targets(t, `{"hooks":"x","mcpServers":"./.mcp.json"}`, ".mcp.json")
		if hooks.Severity != HarnessFail || !strings.Contains(hooks.Evidence, "manifest hooks must be an array") {
			t.Fatalf("hooks = %+v, want the shape FAIL", hooks)
		}
		if mcp.Severity != HarnessFail || !strings.Contains(mcp.Evidence, "not a regular file") {
			t.Fatalf("mcp = %+v, want the unreadable FAIL", mcp)
		}
	})
	t.Run("mcp shape error survives an unreadable hook file", func(t *testing.T) {
		hooks, mcp := harnessRound1Targets(t, `{"hooks":["./hooks/a.json"],"mcpServers":5}`, "hooks/a.json")
		if hooks.Severity != HarnessFail || !strings.Contains(hooks.Evidence, "not a regular file") {
			t.Fatalf("hooks = %+v, want the unreadable FAIL", hooks)
		}
		if mcp.Severity != HarnessFail || !strings.Contains(mcp.Evidence, "mcpServers must be a string file path") {
			t.Fatalf("mcp = %+v, want the shape FAIL", mcp)
		}
	})
	t.Run("a healthy kind passes beside an unreadable one", func(t *testing.T) {
		hooks, mcp := harnessRound1Targets(t, `{"hooks":["./hooks/a.json"]}`, "hooks/a.json")
		if hooks.Severity != HarnessFail || mcp.Severity != HarnessPass {
			t.Fatalf("hooks = %+v mcp = %+v, want FAIL and PASS", hooks, mcp)
		}
	})
	t.Run("findings made before the unreadable file are kept", func(t *testing.T) {
		hooks, _ := harnessRound1Targets(t, `{"hooks":["./hooks/gone.json","./hooks/a.json"]}`, "hooks/a.json")
		if !strings.Contains(hooks.Evidence, "manifest hook file missing: ./hooks/gone.json") || !strings.Contains(hooks.Evidence, "not a regular file") {
			t.Fatalf("hooks = %+v, want the earlier finding and the unreadable file", hooks)
		}
	})
	t.Run("validator keeps its error contract", func(t *testing.T) {
		root := t.TempDir()
		harnessBoundedWrite(t, root, ".codex-plugin/plugin.json", `{"hooks":"x","mcpServers":"./.mcp.json"}`)
		harnessBoundedFIFO(t, filepath.Join(root, ".mcp.json"))
		if issues, err := ValidateManifestTargets(root); err == nil || issues != nil {
			t.Fatalf("issues = %+v err = %v, want only the error", issues, err)
		}
	})
}

// A report field whose read panics is nil; the checks stay.
func TestHarnessRound1MetadataPanicKeepsTheReport(t *testing.T) {
	root := harnessBoundedPayload(t)
	codexHome := filepath.Join(t.TempDir(), "codex")
	if err := os.MkdirAll(codexHome, 0o755); err != nil {
		t.Fatal(err)
	}
	env := harnessRunEnv(map[string]string{"PLUGIN_ROOT": root, "CODEX_HOME": codexHome, "HOME": t.TempDir()})
	allOn := map[string]bool{"multi_agent": true, "goals": true, "hooks": true, "default_mode_request_user_input": true}
	stub := harnessRunStub(allOn, "codex-cli 1.2.3\n")
	runner := func(file string, args []string, timeout time.Duration) HarnessRun {
		if len(args) == 1 && args[0] == "--version" {
			panic("version probe panic")
		}
		return stub(file, args, timeout)
	}
	report, err := harnessRunDoctorRecovered(root, runner, HarnessOptions{}, t.TempDir(), env, time.Now())
	if err != nil {
		t.Fatalf("harnessRunDoctorRecovered: %v, want the report", err)
	}
	if len(report.Checks) == 0 || report.CodexVersion != nil {
		t.Fatalf("report = %+v, want the checks and a nil codex version", report)
	}
}
