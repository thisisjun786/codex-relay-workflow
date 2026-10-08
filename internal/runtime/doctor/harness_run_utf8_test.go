package doctor

// CRW-789 - the real harness runner decodes a probe's streams the way Node does.
//
// The oracle reads a probe's output through spawnSync with encoding utf8
// (plugins/codexclaw/components/cxc-ops/src/doctor.ts, runProbe), so an invalid byte sequence
// another program wrote is one U+FFFD per maximal invalid subpart and can never become a lone
// surrogate. harnessRunExec hands the report that same text (source.DecodeUTF8, the port's Node
// decoder), so harnessReportJSONString cannot spell a probe's invalid byte as the \udXXX escape it
// keeps for the WTF-8 lone surrogate harnessReportCut makes.
//
// Source: CXC v0.2.40 (commit 3c1459acadeb1906d97c00a598e1457327ae372d)
// plugins/codexclaw/components/cxc-ops/src/doctor.ts (the spawnSync encoding utf8 probe) and
// plugins/codexclaw/components/cxc-ops/src/cli.ts:83-84 (the report writer), through
// internal/runtime/doctor/harness_run.go harnessRunExec and harness_report.go.
//
// The decode itself already landed on this base with CRW-683 (PR #719), as a review finding, and no
// existing test drives the real runner's streams. These cases pin it. They are the issue's: the
// bytes 61 ED A0 80 62 from a fake codex are a + three U+FFFD + b in Stderr and in both renderers
// of the report built from that runner; the truncated E2 82 is one U+FFFD; valid UTF-8 and an
// astral character stand as they are; and the CRW-652 pending item this issue carries, a finding
// message that holds a lone surrogate, writes one \udXXX escape in --json and one U+FFFD in text.
//
// Every package-level name carries the harnessRunUTF8 prefix.

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// harnessRunUTF8WTF8 is the three WTF-8 bytes of a lone high surrogate (U+D800): the representation
// pyjson keeps for a JSON \ud800 escape, and the bytes harnessReportCut makes when its 160-unit
// slice splits a surrogate pair. A probe's invalid bytes must never reach a report as this.
const harnessRunUTF8WTF8 = "\xed\xa0\x80"

// harnessRunUTF8SurrogateEscape is the JSON escape a manifest holds for a lone high surrogate: two
// characters of text, which the port's pyjson decodes to harnessRunUTF8WTF8.
const harnessRunUTF8SurrogateEscape = "\\ud800"

// harnessRunUTF8Replacement is what Node's decoder gives the three invalid bytes 61 ED A0 80 62.
const harnessRunUTF8Replacement = "a\uFFFD\uFFFD\uFFFDb"

// harnessRunUTF8FakeCodex writes a stand-in for codex that writes stderr to its own standard error
// and exits status. The bytes travel as printf octal escapes, so the script needs no external
// program and stays exact whatever /bin/sh the host provides.
func harnessRunUTF8FakeCodex(t *testing.T, stderr []byte, status int) string {
	t.Helper()
	dir := t.TempDir()
	var escaped strings.Builder
	for _, b := range stderr {
		fmt.Fprintf(&escaped, "\\%03o", b)
	}
	script := filepath.Join(dir, "codex")
	body := "#!/bin/sh\nprintf '" + escaped.String() + "' >&2\nexit " + strconv.Itoa(status) + "\n"
	syscall.ForkLock.RLock()
	writeErr := os.WriteFile(script, []byte(body), 0o755)
	syscall.ForkLock.RUnlock()
	if writeErr != nil {
		t.Fatal(writeErr)
	}
	return script
}

// harnessRunUTF8IsolatedHome points HOME, CODEX_HOME and CRW_HOME at a temporary directory before
// anything runs, and reports a change in that home's .codex or .crw listing rather than cleaning it
// up (the assignment's real-state rule).
func harnessRunUTF8IsolatedHome(t *testing.T) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CODEX_HOME", filepath.Join(home, ".codex"))
	t.Setenv("CRW_HOME", filepath.Join(home, ".crw"))
	before := harnessRunUTF8HomeListing(home)
	t.Cleanup(func() {
		if after := harnessRunUTF8HomeListing(home); after != before {
			t.Errorf("the isolated home changed: %q -> %q", before, after)
		}
	})
}

// harnessRunUTF8HomeListing is the listing of the isolated home's .codex and .crw.
func harnessRunUTF8HomeListing(home string) string {
	lines := make([]string, 0, 2)
	for _, rel := range []string{".codex", ".crw"} {
		entries, err := os.ReadDir(filepath.Join(home, rel))
		if err != nil {
			lines = append(lines, rel+"=absent")
			continue
		}
		names := make([]string, 0, len(entries))
		for _, entry := range entries {
			names = append(names, entry.Name())
		}
		lines = append(lines, rel+"="+strings.Join(names, ","))
	}
	return strings.Join(lines, " ")
}

// harnessRunUTF8Plugin builds a plugin payload the doctor checks can diagnose: a manifest naming
// crw and one skill with its agents file.
func harnessRunUTF8Plugin(t *testing.T) string {
	t.Helper()
	root := filepath.Join(t.TempDir(), "payload")
	write := func(rel, body string) {
		t.Helper()
		path := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(".codex-plugin", "plugin.json"), "{\"name\":\"crw\",\"version\":\"0.4.0\"}")
	write(filepath.Join("skills", "dev", "SKILL.md"), "---\nname: x\n---\n")
	write(filepath.Join("skills", "dev", "agents", "openai.yaml"), "policy: {}\n")
	return root
}

// TestHarnessRunUTF8ExecDecodesInvalidBytes is the issue's core case: the three bytes ED A0 80 are
// three U+FFFD in the real runner's Stderr, never the raw lone surrogate the report writer would
// spell as an escape, and the exit status is unchanged.
func TestHarnessRunUTF8ExecDecodesInvalidBytes(t *testing.T) {
	harnessRunUTF8IsolatedHome(t)
	script := harnessRunUTF8FakeCodex(t, []byte{0x61, 0xED, 0xA0, 0x80, 0x62}, 3)
	run := harnessRunExec(script, []string{"features", "list"}, 5*time.Second)
	if run.Status == nil || *run.Status != 3 {
		t.Fatalf("status = %v, want 3", run.Status)
	}
	if run.Stderr != harnessRunUTF8Replacement {
		t.Fatalf("stderr = %q (% x); want %q (% x)", run.Stderr, []byte(run.Stderr), harnessRunUTF8Replacement, []byte(harnessRunUTF8Replacement))
	}
	if strings.Contains(run.Stderr, harnessRunUTF8WTF8) {
		t.Fatalf("stderr holds the raw lone surrogate: % x", []byte(run.Stderr))
	}
}

// TestHarnessRunUTF8ExecDecodesTruncatedSequence is the second shape: the truncated three-byte
// character E2 82 at the end of the stream is one U+FFFD (the maximal subpart rule), not two.
func TestHarnessRunUTF8ExecDecodesTruncatedSequence(t *testing.T) {
	harnessRunUTF8IsolatedHome(t)
	script := harnessRunUTF8FakeCodex(t, []byte{0x61, 0xE2, 0x82}, 3)
	run := harnessRunExec(script, []string{"features", "list"}, 5*time.Second)
	if want := "a\uFFFD"; run.Stderr != want {
		t.Fatalf("stderr = %q (% x); want %q", run.Stderr, []byte(run.Stderr), want)
	}
}

// TestHarnessRunUTF8ExecKeepsValidText pins the other side: valid UTF-8 and an astral character
// stand as they are, so the decode rewrites only an invalid subpart.
func TestHarnessRunUTF8ExecKeepsValidText(t *testing.T) {
	harnessRunUTF8IsolatedHome(t)
	want := "a\u00e9\u65e5\u672c\U0001F600b"
	script := harnessRunUTF8FakeCodex(t, []byte(want), 3)
	run := harnessRunExec(script, []string{"features", "list"}, 5*time.Second)
	if run.Stderr != want {
		t.Fatalf("stderr = %q (% x); want %q (% x)", run.Stderr, []byte(run.Stderr), want, []byte(want))
	}
}

// TestHarnessRunUTF8ReportWritesNodeText builds the report from that runner: the features evidence
// and both renderers carry a + three U+FFFD + b, and --json writes no surrogate escape.
func TestHarnessRunUTF8ReportWritesNodeText(t *testing.T) {
	harnessRunUTF8IsolatedHome(t)
	script := harnessRunUTF8FakeCodex(t, []byte{0x61, 0xED, 0xA0, 0x80, 0x62}, 3)
	check := HarnessFeaturesCheck(harnessRunExec(script, []string{"features", "list"}, harnessRunFeaturesTimeout))
	want := "could not read 'codex features list' (exit 3): " + harnessRunUTF8Replacement
	if check.Evidence != want {
		t.Fatalf("evidence = %q (% x); want %q", check.Evidence, []byte(check.Evidence), want)
	}
	report := HarnessReport{SchemaVersion: HarnessSchemaVersion, Overall: check.Severity, Checks: []HarnessCheck{check}}
	var jsonOut bytes.Buffer
	if err := harnessRunWriteJSON(&jsonOut, report); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(jsonOut.String(), harnessRunUTF8Replacement) {
		t.Fatalf("--json does not carry a + three U+FFFD + b:\n%s", jsonOut.String())
	}
	if strings.Contains(jsonOut.String(), "\\ud800") || strings.Contains(jsonOut.String(), harnessRunUTF8WTF8) {
		t.Fatalf("--json spelled the invalid bytes as a surrogate:\n%s", jsonOut.String())
	}
	if text := RenderHarnessReport(report); !strings.Contains(text, harnessRunUTF8Replacement) {
		t.Fatalf("the text report does not carry a + three U+FFFD + b:\n%s", text)
	}
}

// TestHarnessRunUTF8CLIReportMatchesNodeText is the parent's own reproduction: `crw doctor harness
// --json` with a fake codex on PATH, driven through the real harnessRunExec. The features evidence
// ends in three U+FFFD, not in the a\ud800b the issue reported.
func TestHarnessRunUTF8CLIReportMatchesNodeText(t *testing.T) {
	harnessRunUTF8IsolatedHome(t)
	script := harnessRunUTF8FakeCodex(t, []byte{0x61, 0xED, 0xA0, 0x80, 0x62}, 3)
	// Only the fake codex is on PATH, so no check can start a real host program.
	t.Setenv("PATH", filepath.Dir(script))
	root := harnessRunUTF8Plugin(t)
	// The checks read the injected environment, so the report cannot reach the real Codex home.
	values := map[string]string{"PLUGIN_ROOT": root, "CODEX_HOME": filepath.Join(t.TempDir(), ".codex")}
	env := func(key string) (string, bool) { value, ok := values[key]; return value, ok }
	var stdout, stderr bytes.Buffer
	RunHarnessDoctorCLI([]string{"--json"}, &stdout, &stderr, env, func() (string, error) { return t.TempDir(), nil }, harnessRunExec, time.Now())
	if !strings.Contains(stdout.String(), "could not read 'codex features list' (exit 3): "+harnessRunUTF8Replacement) {
		t.Fatalf("--json report does not carry the Node text:\n%s", stdout.String())
	}
	if strings.Contains(stdout.String(), "\\ud800") {
		t.Fatalf("--json report spelled the invalid bytes as an escape:\n%s", stdout.String())
	}
}

// TestHarnessRunUTF8ReportFindingSurrogateWriters is the CRW-652 pending item this issue carries. A
// finding message that holds a lone surrogate - the manifest-target message CRW-652 keeps - writes
// one \ud800 escape in --json and one U+FFFD in text, as JSON.stringify and the UTF-8 encoder do.
func TestHarnessRunUTF8ReportFindingSurrogateWriters(t *testing.T) {
	root := t.TempDir()
	targetTestWrite(t, root, filepath.Join(".codex-plugin", "plugin.json"), "{\"hooks\":[\"./hooks/"+harnessRunUTF8SurrogateEscape+".json\"]}")
	var evidence string
	for _, check := range HarnessManifestTargetChecks(root) {
		if check.Name == "hooks" {
			evidence = check.Evidence
		}
	}
	want := "manifest hook file missing: ./hooks/" + harnessRunUTF8WTF8 + ".json"
	if evidence != want {
		t.Fatalf("evidence = %q (% x); want %q (% x)", evidence, []byte(evidence), want, []byte(want))
	}
	report := HarnessReport{SchemaVersion: HarnessSchemaVersion, Overall: HarnessFail, Checks: []HarnessCheck{{Name: "hooks", Severity: HarnessFail, Evidence: evidence}}}
	var jsonOut bytes.Buffer
	if err := harnessRunWriteJSON(&jsonOut, report); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(jsonOut.String(), "\\ud800") {
		t.Fatalf("--json does not write the \\ud800 escape:\n%s", jsonOut.String())
	}
	if strings.Contains(jsonOut.String(), harnessRunUTF8WTF8) {
		t.Fatalf("--json wrote the raw WTF-8 bytes instead of an escape:\n%s", jsonOut.String())
	}
	text := RenderHarnessReport(report)
	if !strings.Contains(text, "\uFFFD") {
		t.Fatalf("the text report does not write U+FFFD:\n%s", text)
	}
	if strings.Contains(text, harnessRunUTF8WTF8) {
		t.Fatalf("the text report wrote the raw WTF-8 bytes instead of U+FFFD:\n%s", text)
	}
}
