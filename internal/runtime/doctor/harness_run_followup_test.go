package doctor

// This file is the CRW-683 follow-up test for three doctor harness behaviours the CRW-618
// assembly port left to the host: the installed plugin root when PLUGIN_ROOT is absent, the
// lone-surrogate representation of every report string, and the features check's reading of the
// runner's killed marker. It is written red first against the baseline and green after the fix.

import (
	"bytes"
	"encoding/json"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// harnessFollowupCache builds <codexHome>/plugins/cache/<marketplace>/crw/<version> for each
// version, each holding .codex-plugin/plugin.json, and returns the Codex home.
func harnessFollowupCache(t *testing.T, marketplace string, versions ...string) string {
	t.Helper()
	codexHome := t.TempDir()
	for _, version := range versions {
		dir := filepath.Join(codexHome, "plugins", "cache", marketplace, "crw", version, ".codex-plugin")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		body := "{\"name\":\"crw\",\"version\":\"" + version + "\",\"hooks\":[]}"
		if err := os.WriteFile(filepath.Join(dir, "plugin.json"), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return codexHome
}

// harnessFollowupRun drives the command with a temporary working directory and the recorded stub
// runner; the caller supplies the environment (PLUGIN_ROOT, CODEX_HOME). A CODEX_HOME the caller
// does not set is pinned to a temporary directory, so the install-root check never resolves the
// developer's own Codex home.
func harnessFollowupRun(t *testing.T, args []string, values map[string]string) (int, string, string) {
	t.Helper()
	tmp := t.TempDir()
	if _, set := values["CODEX_HOME"]; !set {
		values = maps.Clone(values)
		values["CODEX_HOME"] = filepath.Join(tmp, "codex")
	}
	var stdout, stderr bytes.Buffer
	states := map[string]bool{"multi_agent": true, "goals": true, "hooks": true, "default_mode_request_user_input": true}
	code := RunHarnessDoctorCLI(args, &stdout, &stderr, harnessRunEnv(values), func() (string, error) { return tmp, nil }, harnessRunStub(states, "codex-cli 1.2.3\n"), time.Now())
	return code, stdout.String(), stderr.String()
}

// TestHarnessFollowupInstalledRootWithoutPluginRoot: without PLUGIN_ROOT the command diagnoses the
// single installed crw plugin root under the Codex home's plugin cache, and never the usage exit.
func TestHarnessFollowupInstalledRootWithoutPluginRoot(t *testing.T) {
	codexHome := harnessFollowupCache(t, "m", "0.4.0+x")
	code, stdout, stderr := harnessFollowupRun(t, []string{"--json"}, map[string]string{"CODEX_HOME": codexHome})
	if code == usageExit {
		t.Fatalf("exit = %d (usage); want the installed root diagnosed; stderr %q", code, stderr)
	}
	var report struct {
		PluginVersion *string `json:"pluginVersion"`
	}
	if err := json.Unmarshal([]byte(stdout), &report); err != nil {
		t.Fatalf("stdout is not JSON: %v\n%s", err, stdout)
	}
	if report.PluginVersion == nil || *report.PluginVersion != "0.4.0+x" {
		t.Fatalf("pluginVersion = %v, want the installed root's 0.4.0+x", report.PluginVersion)
	}
}

// TestHarnessFollowupInstalledRootAmbiguous: two version directories are not a root to guess, so
// the command takes the catch path (exit 1) and names the cache directory, the roots found and
// PLUGIN_ROOT.
func TestHarnessFollowupInstalledRootAmbiguous(t *testing.T) {
	codexHome := harnessFollowupCache(t, "m", "0.4.0+x", "0.4.1+y")
	code, stdout, stderr := harnessFollowupRun(t, nil, map[string]string{"CODEX_HOME": codexHome})
	if code != 1 {
		t.Fatalf("exit = %d, want 1; stdout %q stderr %q", code, stdout, stderr)
	}
	if stdout != "" {
		t.Fatalf("stdout = %q, want empty", stdout)
	}
	cacheRoot := filepath.Join(codexHome, "plugins", "cache")
	for _, want := range []string{cacheRoot, "0.4.0+x", "0.4.1+y", "PLUGIN_ROOT"} {
		if !strings.Contains(stderr, want) {
			t.Fatalf("stderr %q does not name %q", stderr, want)
		}
	}
}

// TestHarnessFollowupPluginRootWins: a set PLUGIN_ROOT is the override, whatever the cache holds.
func TestHarnessFollowupPluginRootWins(t *testing.T) {
	tmp := t.TempDir()
	root := harnessRunPayload(t, tmp, "followup-override", harnessRunPayloadOptions{manifest: "{\"name\":\"crw\",\"version\":\"9.9.9\",\"hooks\":[\"./hooks/a.json\"]}"})
	codexHome := harnessFollowupCache(t, "m", "0.4.0+x")
	code, stdout, stderr := harnessFollowupRun(t, []string{"--json"}, map[string]string{"PLUGIN_ROOT": root, "CODEX_HOME": codexHome})
	if code == usageExit {
		t.Fatalf("exit = %d (usage); stderr %q", code, stderr)
	}
	var report struct {
		PluginVersion *string `json:"pluginVersion"`
	}
	if err := json.Unmarshal([]byte(stdout), &report); err != nil {
		t.Fatalf("stdout is not JSON: %v\n%s", err, stdout)
	}
	if report.PluginVersion == nil || *report.PluginVersion != "9.9.9" {
		t.Fatalf("pluginVersion = %v, want the PLUGIN_ROOT payload's 9.9.9", report.PluginVersion)
	}
}

// TestHarnessFollowupReportSurrogateVersion: a lone surrogate the manifest version carries as a
// JSON escape is the lower-case escape JSON.stringify writes under --json and U+FFFD in the text
// report, extending the representation CRW-640 gave the cut stderr.
func TestHarnessFollowupReportSurrogateVersion(t *testing.T) {
	tmp := t.TempDir()
	root := harnessRunPayload(t, tmp, "followup-surrogate", harnessRunPayloadOptions{manifest: "{\"name\":\"crw\",\"version\":\"a\\ud800b\",\"hooks\":[\"./hooks/a.json\"]}"})
	code, stdout, stderr := harnessFollowupRun(t, []string{"--json"}, map[string]string{"PLUGIN_ROOT": root})
	if code == usageExit {
		t.Fatalf("exit = %d (usage); stderr %q", code, stderr)
	}
	if !strings.Contains(stdout, "\\ud800") {
		t.Fatalf("--json output does not carry the surrogate escape:\n%s", stdout)
	}
	if strings.Contains(stdout, "\uFFFD") {
		t.Fatalf("--json output carries U+FFFD instead of the escape:\n%s", stdout)
	}
	textCode, textOut, textErr := harnessFollowupRun(t, nil, map[string]string{"PLUGIN_ROOT": root})
	if textCode == usageExit {
		t.Fatalf("exit = %d (usage); stderr %q", textCode, textErr)
	}
	if !strings.Contains(textOut, "a\uFFFDb") {
		t.Fatalf("text report does not spell the lone surrogate U+FFFD:\n%s", textOut)
	}
}

// TestHarnessFollowupReportSurrogateNameAndSurface: every string field of the report goes through
// the same representation, not only the version header: a check's name and the active surface.
func TestHarnessFollowupReportSurrogateNameAndSurface(t *testing.T) {
	surrogate := harnessReportWTF8HighSurrogate(0x1F600)
	name := "n" + surrogate + "m"
	surface := "s" + surrogate + "t"
	report := HarnessReport{
		SchemaVersion: HarnessSchemaVersion,
		Overall:       HarnessWarn,
		Checks:        []HarnessCheck{{Name: name, Severity: HarnessWarn, Evidence: "e"}},
		ActiveSurface: &surface,
	}
	var buf bytes.Buffer
	if err := harnessRunWriteJSON(&buf, report); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "\\ud83d") {
		t.Fatalf("--json output does not carry the surrogate escape for the name and surface:\n%s", buf.String())
	}
	if strings.Contains(buf.String(), "\uFFFD") {
		t.Fatalf("--json output carries U+FFFD instead of the escape:\n%s", buf.String())
	}
	text := RenderHarnessReport(report)
	if !strings.Contains(text, "n\uFFFDm") {
		t.Fatalf("text report does not spell the check name's lone surrogate U+FFFD:\n%s", text)
	}
}

// TestHarnessFollowupFeaturesKilledIsNullStatus: the runner's killed marker is the oracle's null
// status, so the features check warns without the exit-code phrase; a real non-zero exit keeps it.
func TestHarnessFollowupFeaturesKilledIsNullStatus(t *testing.T) {
	check := HarnessFeaturesCheck(HarnessRun{Status: harnessRunInt(harnessDriftKilled)})
	if check.Severity != HarnessWarn {
		t.Fatalf("severity = %s, want WARN", check.Severity)
	}
	if check.Evidence != "could not read 'codex features list'" {
		t.Fatalf("evidence = %q, want the oracle's null-status text without an exit phrase", check.Evidence)
	}
	real := HarnessFeaturesCheck(HarnessRun{Status: harnessRunInt(3)})
	if !strings.Contains(real.Evidence, "(exit 3)") {
		t.Fatalf("evidence = %q, want the exit phrase for a real exit", real.Evidence)
	}
}

// TestHarnessFollowupChecksKeepMarkupCharacters: the checks array is written with HTML escaping
// off, so an evidence or repair string holding <, > or & keeps the character JSON.stringify writes
// instead of encoding/json's escape (the stale-install repair is the oracle's own text).
func TestHarnessFollowupChecksKeepMarkupCharacters(t *testing.T) {
	report := HarnessReport{
		SchemaVersion: HarnessSchemaVersion,
		Overall:       HarnessFail,
		Checks: []HarnessCheck{{
			Name:     "install-root",
			Severity: HarnessFail,
			Evidence: "mcpServers -> <plugin> & <marketplace>",
			Repair:   harnessReportRepair("codex plugin add <plugin>@<marketplace>"),
		}},
	}
	var buf bytes.Buffer
	if err := harnessRunWriteJSON(&buf, report); err != nil {
		t.Fatal(err)
	}
	got := buf.String()
	for _, want := range []string{"mcpServers -> <plugin> & <marketplace>", "codex plugin add <plugin>@<marketplace>"} {
		if !strings.Contains(got, want) {
			t.Fatalf("--json output does not carry %q literally:\n%s", want, got)
		}
	}
	for _, escaped := range []string{"\\u003c", "\\u003e", "\\u0026"} {
		if strings.Contains(got, escaped) {
			t.Fatalf("--json output HTML-escapes the checks:\n%s", got)
		}
	}
}

// TestHarnessFollowupEnvironmentBytesDecodeLikeNode: an environment value is bytes Node's UTF-8
// decoder already read, so the raw ED A0 80 of a CODEX_SURFACE becomes three U+FFFD in both the
// JSON and the text report -- never the \ud800 escape a stored lone surrogate gets.
func TestHarnessFollowupEnvironmentBytesDecodeLikeNode(t *testing.T) {
	raw := string([]byte{0xED, 0xA0, 0x80})
	env := harnessRunEnv(map[string]string{"CODEX_SURFACE": "s" + raw + "t"})
	surface := harnessRunActiveSurface(env)
	if surface == nil {
		t.Fatal("activeSurface is nil, want the decoded value")
	}
	if *surface != "s\uFFFD\uFFFD\uFFFDt" {
		t.Fatalf("activeSurface = %q, want the three U+FFFD Node's decoder writes", *surface)
	}
	report := HarnessReport{SchemaVersion: HarnessSchemaVersion, Overall: HarnessWarn, Checks: []HarnessCheck{}, ActiveSurface: surface}
	var buf bytes.Buffer
	if err := harnessRunWriteJSON(&buf, report); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(buf.String(), "\\ud800") {
		t.Fatalf("--json output spells the raw bytes as a lone surrogate:\n%s", buf.String())
	}
	if !strings.Contains(buf.String(), "s\uFFFD\uFFFD\uFFFDt") {
		t.Fatalf("--json output does not carry the three U+FFFD:\n%s", buf.String())
	}
}

// TestHarnessFollowupUnreadableCacheIsNotGuessed: a cache directory the scan cannot read may hold
// another installed root, so the command refuses instead of diagnosing the ones it can see.
func TestHarnessFollowupUnreadableCacheIsNotGuessed(t *testing.T) {
	codexHome := harnessFollowupCache(t, "public", "0.4.0+x")
	hidden := filepath.Join(codexHome, "plugins", "cache", "local", "crw")
	if err := os.MkdirAll(hidden, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(hidden, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(hidden, 0o755) })
	if _, err := os.ReadDir(hidden); err == nil {
		t.Skip("the cache directory stays readable despite mode 000 (privileged user)")
	}
	code, stdout, stderr := harnessFollowupRun(t, nil, map[string]string{"CODEX_HOME": codexHome})
	if code != 1 {
		t.Fatalf("exit = %d, want 1; stdout %q stderr %q", code, stdout, stderr)
	}
	if stdout != "" {
		t.Fatalf("stdout = %q, want empty", stdout)
	}
	if !strings.Contains(stderr, "cannot read") || !strings.Contains(stderr, "PLUGIN_ROOT") {
		t.Fatalf("stderr = %q, want the unreadable-cache refusal naming PLUGIN_ROOT", stderr)
	}
}
