package doctor

// CRW-789 generation 2 (scope_change) - the retrust verification reads codex's output the way
// Node does.
//
// The oracle's verifyCodexConfig (CXC v0.2.40
// plugins/codexclaw/components/cxc-ops/src/hook-trust.ts:387-410) calls the runner with spawnSync
// options {encoding: "utf8", ...} (:398), so its stdout and stderr are already-decoded strings and
// an invalid byte sequence is one U+FFFD per maximal invalid subpart. hookTrustRetrustExec is the
// place of that call, and it must hand every consumer the same text: hookTrustRetrustVerify's
// "codex features list verification failed: <detail>" is printed by crw doctor retrust, so raw
// bytes there reach a person's terminal where the oracle writes U+FFFD.
//
// Red first on dev: the seam returns stdout.String() and stderr.String(), so the failure text
// carries the raw 61 ED A0 80 62. Green once both streams go through source.DecodeUTF8.
//
// Every package-level name carries the hookTrustRetrustUTF8 prefix.

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// hookTrustRetrustUTF8Replacement is what Node's decoder gives the three invalid bytes
// 61 ED A0 80 62: a, three U+FFFD and b.
const hookTrustRetrustUTF8Replacement = "a\uFFFD\uFFFD\uFFFDb"

// hookTrustRetrustUTF8FakeCodex writes a stand-in for codex that writes stderr to its own standard
// error and exits status. The bytes travel as printf octal escapes, so the script needs no external
// program and stays exact whatever /bin/sh the host provides.
func hookTrustRetrustUTF8FakeCodex(t *testing.T, stderr []byte, status int) string {
	t.Helper()
	dir := t.TempDir()
	var escaped strings.Builder
	for _, b := range stderr {
		fmt.Fprintf(&escaped, "\\%03o", b)
	}
	script := filepath.Join(dir, "codex")
	body := "#!/bin/sh\nprintf '" + escaped.String() + "' >&2\nexit " + fmt.Sprint(status) + "\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	return script
}

// hookTrustRetrustUTF8IsolatedHome points HOME, CODEX_HOME and CRW_HOME at a temporary directory
// before anything runs, and reports a change in that home's .codex or .crw listing rather than
// cleaning it up (the assignment's real-state rule).
func hookTrustRetrustUTF8IsolatedHome(t *testing.T) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CODEX_HOME", filepath.Join(home, ".codex"))
	t.Setenv("CRW_HOME", filepath.Join(home, ".crw"))
	// The verification resolves codex through CODEX_BIN before PATH
	// (hookTrustRetrustCodexBinary, codex_bin.go:33-36), so an inherited non-blank
	// CODEX_BIN would send the probe to an externally configured binary instead of the
	// fake codex these cases put on PATH. A blank value does not override, which keeps
	// the cases hermetic wherever they run.
	t.Setenv("CODEX_BIN", "")
	before := hookTrustRetrustUTF8HomeListing(home)
	t.Cleanup(func() {
		if after := hookTrustRetrustUTF8HomeListing(home); after != before {
			t.Errorf("the isolated home changed: %q -> %q", before, after)
		}
	})
}

// hookTrustRetrustUTF8HomeListing is the listing of the isolated home's .codex and .crw.
func hookTrustRetrustUTF8HomeListing(home string) string {
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

// TestHookTrustRetrustUTF8ExecDecodesStreams is the widened decode itself: the real runner answers
// the invalid bytes as three U+FFFD and keeps the exit status.
func TestHookTrustRetrustUTF8ExecDecodesStreams(t *testing.T) {
	hookTrustRetrustUTF8IsolatedHome(t)
	script := hookTrustRetrustUTF8FakeCodex(t, []byte{0x61, 0xED, 0xA0, 0x80, 0x62}, 1)
	run := hookTrustRetrustExec(script, []string{"features", "list"}, os.Environ())
	if run.Status == nil || *run.Status != 1 {
		t.Fatalf("status = %v, want 1", run.Status)
	}
	if run.Stderr != hookTrustRetrustUTF8Replacement {
		t.Fatalf("stderr = %q (% x); want %q (% x)", run.Stderr, []byte(run.Stderr), hookTrustRetrustUTF8Replacement, []byte(hookTrustRetrustUTF8Replacement))
	}
	if strings.Contains(run.Stderr, "\xed\xa0\x80") {
		t.Fatalf("stderr kept the raw lone surrogate: % x", []byte(run.Stderr))
	}
}

// TestHookTrustRetrustUTF8VerifyFailureTextIsNodeText is the criterion's case: a fake codex that
// fails the retrust verification with those bytes makes the reported failure text carry
// a + three U+FFFD + b, where dev carries the raw bytes.
func TestHookTrustRetrustUTF8VerifyFailureTextIsNodeText(t *testing.T) {
	hookTrustRetrustUTF8IsolatedHome(t)
	script := hookTrustRetrustUTF8FakeCodex(t, []byte{0x61, 0xED, 0xA0, 0x80, 0x62}, 1)
	t.Setenv("PATH", filepath.Dir(script))
	err := hookTrustRetrustVerify(t.TempDir(), hookTrustRetrustExec)
	if err == nil {
		t.Fatal("the failing probe was accepted")
	}
	want := "codex features list verification failed: " + hookTrustRetrustUTF8Replacement
	if err.Error() != want {
		t.Fatalf("error = %q (% x); want %q", err.Error(), []byte(err.Error()), want)
	}
	if strings.Contains(err.Error(), "\xed\xa0\x80") {
		t.Fatalf("the failure text kept the raw lone surrogate: % x", []byte(err.Error()))
	}
}

// TestHookTrustRetrustUTF8VerifyFailureTextThroughCLI drives the same failure through the command,
// so the text a person reads is the decoded one end to end.
func TestHookTrustRetrustUTF8VerifyFailureTextThroughCLI(t *testing.T) {
	hookTrustRetrustUTF8IsolatedHome(t)
	script := hookTrustRetrustUTF8FakeCodex(t, []byte{0x61, 0xED, 0xA0, 0x80, 0x62}, 1)
	t.Setenv("PATH", filepath.Dir(script))
	root := t.TempDir()
	plugin := filepath.Join(root, "plugin")
	if err := os.MkdirAll(filepath.Join(plugin, ".codex-plugin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(plugin, "hooks"), 0o755); err != nil {
		t.Fatal(err)
	}
	hookTrustRetrustUTF8Write(t, filepath.Join(plugin, ".codex-plugin", "plugin.json"), `{"name":"crw","hooks":["./hooks/one.json"]}`+"\n")
	hookTrustRetrustUTF8Write(t, filepath.Join(plugin, "hooks", "one.json"), `{"hooks":{"Stop":[{"hooks":[{"type":"command","command":"echo one"}]}]}}`+"\n")
	home := filepath.Join(root, "codex")
	if err := os.MkdirAll(home, 0o755); err != nil {
		t.Fatal(err)
	}
	hookTrustRetrustUTF8Write(t, filepath.Join(home, "config.toml"), "model = \"gpt-5.5\"\n\n[plugins.\"crw@local\"]\nenabled = true\n")
	values := map[string]string{"HOME": root, "CODEX_HOME": home, "CRW_HOME": filepath.Join(root, "crw"), "PATH": filepath.Dir(script)}
	env := func(key string) (string, bool) { value, ok := values[key]; return value, ok }
	var stdout, stderr strings.Builder
	code := HookTrustRetrustCLI([]string{"--bootstrap-ok"}, &stdout, &stderr, env, hookTrustRetrustExec, plugin, hookTrustRetrustUTF8Now())
	if code != 1 {
		t.Fatalf("exit = %d, want 1; stderr = %q", code, stderr.String())
	}
	want := "codex features list verification failed: " + hookTrustRetrustUTF8Replacement
	if !strings.Contains(stderr.String(), want) {
		t.Fatalf("stderr = %q (% x); want it to carry %q", stderr.String(), []byte(stderr.String()), want)
	}
	if strings.Contains(stderr.String(), "\xed\xa0\x80") {
		t.Fatalf("the printed failure text kept the raw lone surrogate: % x", []byte(stderr.String()))
	}
}

// hookTrustRetrustUTF8Write writes one fixture file, creating its directory.
func hookTrustRetrustUTF8Write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// hookTrustRetrustUTF8Now is a fixed instant so the backup name does not depend on the clock.
func hookTrustRetrustUTF8Now() time.Time {
	return time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
}

// hookTrustRetrustUTF8StartFailureIsUnchanged pins that the decode touches only the streams: a
// binary that cannot start still answers the spawn error and a nil status.
func TestHookTrustRetrustUTF8StartFailureIsUnchanged(t *testing.T) {
	hookTrustRetrustUTF8IsolatedHome(t)
	run := hookTrustRetrustExec(filepath.Join(t.TempDir(), "nope"), nil, os.Environ())
	if run.Status != nil {
		t.Fatalf("a missing binary answered status %v, want nil", *run.Status)
	}
	if run.Error == "" {
		t.Fatal("a missing binary answered no spawn error")
	}
	if run.Stdout != "" || run.Stderr != "" {
		t.Fatalf("a missing binary answered streams %q %q", run.Stdout, run.Stderr)
	}
}
