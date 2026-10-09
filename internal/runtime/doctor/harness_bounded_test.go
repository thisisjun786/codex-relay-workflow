package doctor

// CRW-1152 (A7-06): a check of the harness report that cannot read what it inspects reports it
// instead of panicking, a record the harness does not control is opened without blocking, judged on
// the descriptor, and read within a byte and depth bound, and the manifest-target validator checks
// the declared types. Written red first against the baseline.

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// harnessBoundedBudget is the time any check below may take. A read that blocks on a FIFO never
// returns, so the call runs in a goroutine and the test fails at the budget.
const harnessBoundedBudget = 5 * time.Second

func harnessBoundedWithin[T any](t *testing.T, what string, run func() T) T {
	t.Helper()
	done := make(chan T, 1)
	thrown := make(chan any, 1)
	go func() {
		defer func() {
			if value := recover(); value != nil {
				thrown <- value
			}
		}()
		done <- run()
	}()
	select {
	case got := <-done:
		return got
	case value := <-thrown:
		t.Fatalf("%s panicked: %v", what, value)
		panic("unreachable")
	case <-time.After(harnessBoundedBudget):
		t.Fatalf("%s did not finish within %s", what, harnessBoundedBudget)
		panic("unreachable")
	}
}

func harnessBoundedFIFO(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(path, 0o644); err != nil {
		t.Skipf("mkfifo: %v", err)
	}
}

func harnessBoundedWrite(t *testing.T, root, rel, body string) {
	t.Helper()
	path := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func harnessBoundedCheck(checks []HarnessCheck, name string) (HarnessCheck, bool) {
	for _, check := range checks {
		if check.Name == name {
			return check, true
		}
	}
	return HarnessCheck{}, false
}

func harnessBoundedLocked(t *testing.T, dir string) {
	t.Helper()
	if err := os.Chmod(dir, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })
	if _, err := os.ReadDir(dir); err == nil {
		t.Skip("the directory stays readable despite mode 000 (privileged user)")
	}
}

// harnessBoundedReport runs the CLI as `crw doctor harness --json` over a payload and a project.
func harnessBoundedReport(t *testing.T, root, project string) (int, string, string, map[string]any) {
	t.Helper()
	codexHome := filepath.Join(t.TempDir(), "codex")
	if err := os.MkdirAll(codexHome, 0o755); err != nil {
		t.Fatal(err)
	}
	env := harnessRunEnv(map[string]string{"PLUGIN_ROOT": root, "CODEX_HOME": codexHome, "HOME": t.TempDir()})
	allOn := map[string]bool{"multi_agent": true, "goals": true, "hooks": true, "default_mode_request_user_input": true}
	var stdout, stderr bytes.Buffer
	code := RunHarnessDoctorCLI([]string{"--json"}, &stdout, &stderr, env, func() (string, error) { return project, nil }, harnessRunStub(allOn, "codex-cli 1.2.3\n"), time.Now())
	var report map[string]any
	if stdout.Len() > 0 {
		if err := json.Unmarshal(stdout.Bytes(), &report); err != nil {
			t.Fatalf("stdout is not JSON: %v\n%s", err, stdout.String())
		}
	}
	return code, stdout.String(), stderr.String(), report
}

func harnessBoundedReportCheck(t *testing.T, report map[string]any, name string) map[string]any {
	t.Helper()
	checks, _ := report["checks"].([]any)
	for _, raw := range checks {
		if check, _ := raw.(map[string]any); check["name"] == name {
			return check
		}
	}
	t.Fatalf("the report holds no %q check: %v", name, report)
	return nil
}

func harnessBoundedPayload(t *testing.T) string {
	t.Helper()
	return harnessRunPayload(t, t.TempDir(), "bounded", harnessRunPayloadOptions{manifest: "{\"name\":\"crw\",\"version\":\"0.0.1\",\"hooks\":[\"./hooks/a.json\"],\"mcpServers\":\"./.mcp.json\"}"})
}

// Criterion 1: an unreadable sessions directory leaves the rest of the report.
func TestHarnessBoundedUnreadableSessionsKeepsTheReport(t *testing.T) {
	root := harnessBoundedPayload(t)
	project := t.TempDir()
	harnessBoundedWrite(t, project, ".crw/sessions/s.json", "{}")
	harnessBoundedLocked(t, filepath.Join(project, ".crw", "sessions"))
	code, stdout, stderr, report := harnessBoundedReport(t, root, project)
	if stdout == "" || report == nil {
		t.Fatalf("exit %d, stdout empty, stderr %q: the report was lost", code, stderr)
	}
	if report["schemaVersion"] == nil {
		t.Fatalf("no schemaVersion in %s", stdout)
	}
	pabcd := harnessBoundedReportCheck(t, report, "pabcd-state")
	if pabcd["severity"] != "WARN" || !strings.Contains(pabcd["evidence"].(string), "EACCES") {
		t.Fatalf("pabcd-state = %v, want a WARN naming the unreadable directory", pabcd)
	}
	for _, name := range []string{"manifest", "skills", "agents", "install-root"} {
		harnessBoundedReportCheck(t, report, name)
	}
}

func TestHarnessBoundedUnreadableSkillsAndAgentsKeepTheReport(t *testing.T) {
	root := harnessBoundedPayload(t)
	harnessBoundedLocked(t, filepath.Join(root, "skills"))
	harnessBoundedLocked(t, filepath.Join(root, "agents"))
	code, stdout, stderr, report := harnessBoundedReport(t, root, t.TempDir())
	if report == nil {
		t.Fatalf("exit %d, stdout %q, stderr %q: the report was lost", code, stdout, stderr)
	}
	for _, name := range []string{"skills", "agents"} {
		check := harnessBoundedReportCheck(t, report, name)
		if check["severity"] != "WARN" || !strings.Contains(check["evidence"].(string), "EACCES") {
			t.Fatalf("%s = %v, want a WARN naming the unreadable directory", name, check)
		}
	}
	harnessBoundedReportCheck(t, report, "pabcd-state")
}

// A home that cannot be established is a skipped install-root check, not a lost report.
func TestHarnessBoundedUnestablishedHomeIsAWarn(t *testing.T) {
	read := false
	check := harnessBoundedWithin(t, "the install-root check", func() HarnessCheck {
		return harnessInstallInstalledRootCheck("/payload", func() (string, error) { return "", os.ErrInvalid },
			func(string) ([]byte, error) { read = true; return nil, os.ErrInvalid })
	})
	if check.Name != "install-root" || check.Severity != HarnessWarn || check.Evidence == "" {
		t.Fatalf("check = %+v, want the install-root WARN", check)
	}
	if read {
		t.Fatal("the manifest was read although no home could be established")
	}
}

// A check that panics anyway is one WARN, not the loss of the checks around it.
func TestHarnessBoundedAPanickingCheckIsContained(t *testing.T) {
	got := harnessBoundedWithin(t, "the guard", func() []HarnessCheck { return harnessRunGuard("boom", func() []HarnessCheck { panic("kaboom") }) })
	if len(got) != 1 || got[0].Name != "boom" || got[0].Severity != HarnessWarn || !strings.Contains(got[0].Evidence, "kaboom") {
		t.Fatalf("got %+v, want one WARN named boom carrying the message", got)
	}
}

// Criterion 2: a FIFO record is reported inside the budget.
func TestHarnessBoundedFIFOSessionSlot(t *testing.T) {
	project := t.TempDir()
	harnessBoundedWrite(t, project, ".crw/sessions/ok.json", "{}")
	harnessBoundedFIFO(t, filepath.Join(project, ".crw", "sessions", "pipe.json"))
	check := harnessBoundedWithin(t, "the pabcd check", func() HarnessCheck { return HarnessPabcdCheck(project) })
	if check.Severity != HarnessWarn || !strings.Contains(check.Evidence, "pipe.json") || strings.Contains(check.Evidence, "ok.json") {
		t.Fatalf("check = %+v, want a WARN naming pipe.json only", check)
	}
}

func TestHarnessBoundedLargeAndDeepSessionSlots(t *testing.T) {
	project := t.TempDir()
	harnessBoundedWrite(t, project, ".crw/sessions/ok.json", "{}")
	harnessBoundedWrite(t, project, ".crw/sessions/big.json", `{"pad":"`+strings.Repeat("x", harnessReadLimit)+`"}`)
	harnessBoundedWrite(t, project, ".crw/sessions/deep.json", strings.Repeat("[", 10001)+strings.Repeat("]", 10001))
	check := harnessBoundedWithin(t, "the pabcd check", func() HarnessCheck { return HarnessPabcdCheck(project) })
	if check.Severity != HarnessWarn || !strings.Contains(check.Evidence, "big.json") || !strings.Contains(check.Evidence, "deep.json") || strings.Contains(check.Evidence, "ok.json") {
		t.Fatalf("check = %+v, want a WARN naming big.json and deep.json", check)
	}
}

// A link that stays inside the sessions directory is still followed.
func TestHarnessBoundedSessionLinkIsFollowed(t *testing.T) {
	project := t.TempDir()
	harnessBoundedWrite(t, project, ".crw/sessions/real.json", "{}")
	if err := os.Symlink("real.json", filepath.Join(project, ".crw", "sessions", "link.json")); err != nil {
		t.Fatal(err)
	}
	check := HarnessPabcdCheck(project)
	if check.Severity != HarnessPass || !strings.Contains(check.Evidence, "2 session file(s)") {
		t.Fatalf("check = %+v, want PASS over both", check)
	}
}

func TestHarnessBoundedFIFOManifestAndHookFiles(t *testing.T) {
	t.Run("plugin.json", func(t *testing.T) {
		root := t.TempDir()
		harnessBoundedFIFO(t, filepath.Join(root, ".codex-plugin", "plugin.json"))
		check := harnessBoundedWithin(t, "the manifest check", func() HarnessCheck {
			return harnessRunManifestCheck(filepath.Join(root, ".codex-plugin", "plugin.json"))
		})
		if check.Severity != HarnessFail {
			t.Fatalf("check = %+v, want FAIL", check)
		}
		checks := harnessBoundedWithin(t, "the target checks", func() []HarnessCheck { return HarnessManifestTargetChecks(root) })
		if len(checks) == 0 || checks[0].Severity != HarnessFail {
			t.Fatalf("checks = %+v, want a FAIL", checks)
		}
		if version := harnessBoundedWithin(t, "the version read", func() *string { return harnessRunPluginVersion(filepath.Join(root, ".codex-plugin", "plugin.json")) }); version != nil {
			t.Fatalf("version = %q, want nil", *version)
		}
		if err := harnessBoundedWithin(t, "the trust listing", func() error { _, err := ListHookTrustEntries(root, "k@m"); return err }); err == nil {
			t.Fatal("ListHookTrustEntries read a FIFO manifest")
		}
	})
	t.Run("hook file", func(t *testing.T) {
		root := t.TempDir()
		harnessBoundedWrite(t, root, ".codex-plugin/plugin.json", `{"name":"crw","version":"1","hooks":["./hooks/a.json"]}`)
		harnessBoundedFIFO(t, filepath.Join(root, "hooks", "a.json"))
		checks := harnessBoundedWithin(t, "the target checks", func() []HarnessCheck { return HarnessManifestTargetChecks(root) })
		hooks, _ := harnessBoundedCheck(checks, "hooks")
		if hooks.Severity != HarnessFail || !strings.Contains(hooks.Evidence, "hooks/a.json") {
			t.Fatalf("checks = %+v, want a hooks FAIL naming the file", checks)
		}
		if err := harnessBoundedWithin(t, "the trust listing", func() error { _, err := ListHookTrustEntries(root, "k@m"); return err }); err == nil || !strings.Contains(err.Error(), "not a regular file") {
			t.Fatalf("ListHookTrustEntries err = %v, want not a regular file", err)
		}
	})
	t.Run("mcp file", func(t *testing.T) {
		root := t.TempDir()
		harnessBoundedWrite(t, root, ".codex-plugin/plugin.json", `{"name":"crw","version":"1","mcpServers":"./.mcp.json"}`)
		harnessBoundedFIFO(t, filepath.Join(root, ".mcp.json"))
		checks := harnessBoundedWithin(t, "the target checks", func() []HarnessCheck { return HarnessManifestTargetChecks(root) })
		mcp, _ := harnessBoundedCheck(checks, "mcp-targets")
		if mcp.Severity != HarnessFail {
			t.Fatalf("checks = %+v, want an mcp-targets FAIL", checks)
		}
		drift := harnessBoundedWithin(t, "the drift checks", func() []HarnessCheck { return HarnessDriftChecks(root) })
		if check, ok := harnessBoundedCheck(drift, "drift:mcp"); !ok || check.Severity != HarnessFail {
			t.Fatalf("drift = %+v, want a drift:mcp FAIL", drift)
		}
	})
}

func TestHarnessBoundedLargeAndDeepDocuments(t *testing.T) {
	for name, body := range map[string]string{
		"large": `{"name":"crw","version":"1","pad":"` + strings.Repeat("x", harnessReadLimit) + `"}`,
		"deep":  strings.Repeat("[", 10001) + strings.Repeat("]", 10001),
	} {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			harnessBoundedWrite(t, root, ".codex-plugin/plugin.json", body)
			path := filepath.Join(root, ".codex-plugin", "plugin.json")
			if check := harnessRunManifestCheck(path); check.Severity != HarnessFail {
				t.Fatalf("manifest check = %+v, want FAIL, not PASS or absent", check)
			}
			checks := HarnessManifestTargetChecks(root)
			if len(checks) == 0 || checks[0].Severity != HarnessFail {
				t.Fatalf("target checks = %+v, want a FAIL", checks)
			}
		})
	}
	t.Run("large hook file", func(t *testing.T) {
		root := t.TempDir()
		harnessBoundedWrite(t, root, ".codex-plugin/plugin.json", `{"name":"crw","version":"1","hooks":["./hooks/a.json"]}`)
		harnessBoundedWrite(t, root, "hooks/a.json", `{"hooks":{},"pad":"`+strings.Repeat("x", harnessReadLimit)+`"}`)
		checks := HarnessManifestTargetChecks(root)
		hooks, _ := harnessBoundedCheck(checks, "hooks")
		if hooks.Severity != HarnessFail {
			t.Fatalf("checks = %+v, want a hooks FAIL", checks)
		}
		// The harness lists within the bound; the exported listing the retrust command shares reads whole.
		if _, err := listHookTrustEntries(root, "k@m", harnessReadLimit); !errors.Is(err, errHarnessTooLarge) {
			t.Fatalf("the bounded listing read a hook file past the limit: %v", err)
		}
		if _, err := ListHookTrustEntries(root, "k@m"); err != nil {
			t.Fatalf("ListHookTrustEntries refused a valid hook file: %v", err)
		}
	})
}

// Criterion 3: declared types and event membership.
func TestHarnessBoundedTargetTypes(t *testing.T) {
	issues := func(t *testing.T, manifest string, files map[string]string) []TargetIssue {
		t.Helper()
		root := t.TempDir()
		harnessBoundedWrite(t, root, ".codex-plugin/plugin.json", manifest)
		for rel, body := range files {
			harnessBoundedWrite(t, root, rel, body)
		}
		got, err := ValidateManifestTargets(root)
		if err != nil {
			t.Fatalf("ValidateManifestTargets: %v", err)
		}
		return got
	}
	has := func(got []TargetIssue, kind TargetKind, text string) bool {
		for _, issue := range got {
			if issue.Kind == kind && strings.Contains(issue.Message, text) {
				return true
			}
		}
		return false
	}
	t.Run("a non-empty directory is not a file target", func(t *testing.T) {
		got := issues(t, `{"hooks":["./hooks/a.json"],"mcpServers":"./.mcp.json"}`, map[string]string{
			"hooks/a.json":  `{"hooks":{"Stop":[{"hooks":[{"command":"node ${PLUGIN_ROOT}/dist/run.js"}]}]}}`,
			"dist/run.js/x": "x",
			".mcp.json":     `{"mcpServers":{"s":{"args":["./srv.js"]}}}`,
			"srv.js/y":      "y",
		})
		if !has(got, TargetHook, "not a regular file: dist/run.js") || !has(got, TargetMCP, "not a regular file: ./srv.js") {
			t.Fatalf("issues = %+v, want both directories refused", got)
		}
	})
	t.Run("a FIFO target is not a file target", func(t *testing.T) {
		root := t.TempDir()
		harnessBoundedWrite(t, root, ".codex-plugin/plugin.json", `{"hooks":["./hooks/a.json"]}`)
		harnessBoundedWrite(t, root, "hooks/a.json", `{"hooks":{"Stop":[{"hooks":[{"command":"node ${PLUGIN_ROOT}/dist/run.js"}]}]}}`)
		harnessBoundedFIFO(t, filepath.Join(root, "dist", "run.js"))
		got := harnessBoundedWithin(t, "the validation", func() []TargetIssue { got, _ := ValidateManifestTargets(root); return got })
		if !has(got, TargetHook, "not a regular file: dist/run.js") {
			t.Fatalf("issues = %+v", got)
		}
	})
	t.Run("a link to a regular file is a file target", func(t *testing.T) {
		root := t.TempDir()
		harnessBoundedWrite(t, root, ".codex-plugin/plugin.json", `{"hooks":["./hooks/a.json"]}`)
		harnessBoundedWrite(t, root, "hooks/a.json", `{"hooks":{"Stop":[{"hooks":[{"command":"node ${PLUGIN_ROOT}/dist/run.js"}]}]}}`)
		harnessBoundedWrite(t, root, "dist/real.js", "x")
		if err := os.Symlink("real.js", filepath.Join(root, "dist", "run.js")); err != nil {
			t.Fatal(err)
		}
		if got, err := ValidateManifestTargets(root); err != nil || len(got) != 0 {
			t.Fatalf("issues = %+v, %v, want none", got, err)
		}
	})
	t.Run("hooks that is not an array", func(t *testing.T) {
		for _, hooks := range []string{`"x"`, `5`, `{}`, `true`} {
			got := issues(t, `{"hooks":`+hooks+`}`, nil)
			if !has(got, TargetHook, "manifest hooks must be an array") {
				t.Fatalf("hooks %s: issues = %+v", hooks, got)
			}
		}
		if got := issues(t, `{"hooks":null}`, nil); len(got) != 0 {
			t.Fatalf("hooks null: issues = %+v, want none", got)
		}
		if got := issues(t, `{}`, nil); len(got) != 0 {
			t.Fatalf("no hooks: issues = %+v, want none", got)
		}
	})
	t.Run("mcpServers that is not a string", func(t *testing.T) {
		for _, mcp := range []string{`5`, `{}`, `["./.mcp.json"]`, `true`} {
			got := issues(t, `{"mcpServers":`+mcp+`}`, nil)
			if !has(got, TargetMCP, "manifest mcpServers must be a string") {
				t.Fatalf("mcpServers %s: issues = %+v", mcp, got)
			}
		}
		if got := issues(t, `{"mcpServers":null}`, nil); len(got) != 0 {
			t.Fatalf("mcpServers null: issues = %+v, want none", got)
		}
	})
	t.Run("hooks and mcpServers members of the declared files", func(t *testing.T) {
		got := issues(t, `{"hooks":["./hooks/a.json"],"mcpServers":"./.mcp.json"}`, map[string]string{
			"hooks/a.json": `{"hooks":"x"}`,
			".mcp.json":    `{"mcpServers":5}`,
		})
		if !has(got, TargetHook, "hooks must be an object: ./hooks/a.json") || !has(got, TargetMCP, "mcpServers must be an object: ./.mcp.json") {
			t.Fatalf("issues = %+v", got)
		}
	})
	t.Run("an Object.prototype event", func(t *testing.T) {
		for _, event := range []string{"constructor", "toString", "__proto__", "hasOwnProperty"} {
			got := issues(t, `{"hooks":["./hooks/a.json"]}`, map[string]string{
				"hooks/a.json": `{"hooks":{"` + event + `":[]}}`,
			})
			if !has(got, TargetHook, "hook event is not supported: "+event) {
				t.Fatalf("event %s: issues = %+v", event, got)
			}
		}
	})
}

// The trust identity refuses what is not one of the ten event labels.
func TestHarnessBoundedPrototypeEventsAreRefused(t *testing.T) {
	handler := map[string]any{"type": "command", "command": "echo ok"}
	for _, event := range []string{"constructor", "toString", "valueOf", "hasOwnProperty", "isPrototypeOf", "propertyIsEnumerable", "toLocaleString", "__defineGetter__", "__defineSetter__", "__lookupGetter__", "__lookupSetter__", "__proto__"} {
		if got, err := HookTrustIdentityHash(event, nil, handler); err == nil || err.Error() != "unsupported hook event: "+event {
			t.Fatalf("HookTrustIdentityHash(%q) = %q, %v, want the unsupported-event refusal", event, got, err)
		}
		root := t.TempDir()
		harnessBoundedWrite(t, root, ".codex-plugin/plugin.json", `{"name":"x","hooks":["./hooks/a.json"]}`)
		harnessBoundedWrite(t, root, "hooks/a.json", `{"hooks":{"`+event+`":[{"hooks":[{"type":"command","command":"echo ok"}]}]}}`)
		if _, err := ListHookTrustEntries(root, "x@m"); err == nil || err.Error() != "unsupported hook event: "+event {
			t.Fatalf("ListHookTrustEntries(%q) err = %v, want the unsupported-event refusal", event, err)
		}
	}
	// The ten event labels keep their hash.
	if _, err := HookTrustIdentityHash("Stop", nil, handler); err != nil {
		t.Fatalf("Stop: %v", err)
	}
}
