package shadow_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/flow/shadow"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/trace"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/trace/tracetest"
)

// crw runs `crw flow args...` in-process and returns the status and both streams.
func crw(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	var out, errb bytes.Buffer
	code := shadow.RunCLI(tracetest.Context(t), args, &out, &errb)
	return code, out.String(), errb.String()
}

func script(name string) string { return filepath.Join(dataDir, name) }

// TestShadowLeavesCopyUnchanged runs all three commands over a store copy and checks that nothing under the copy's directory
// was written, created (no write-ahead log, no shared-memory file), removed or touched, and that the SHA-256 of the copy is the
// one the trace names.
func TestShadowLeavesCopyUnchanged(t *testing.T) {
	s := tracetest.NewScenario(tracetest.New(t))
	copyPath := s.Copy()
	dir := filepath.Dir(copyPath)
	before := tracetest.Snapshot(t, dir)
	data, err := os.ReadFile(copyPath)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(data)

	out := t.TempDir()
	tracePath := filepath.Join(out, "trace.json")
	if code, _, stderr := crw(t, "trace", "export", "--store-copy", copyPath, "--out", tracePath); code != 0 {
		t.Fatalf("trace export = %d: %s", code, stderr)
	}
	reportPath := filepath.Join(out, "shadow.json")
	if code, _, stderr := crw(t, "shadow", "--trace", tracePath, "--script", script("recorded.js"), "--out", reportPath); code != 0 {
		t.Fatalf("shadow = %d: %s", code, stderr)
	}
	measurePath := filepath.Join(out, "measure.json")
	if code, _, stderr := crw(t, "measure", "--trace", tracePath, "--out", measurePath); code != 0 {
		t.Fatalf("measure = %d: %s", code, stderr)
	}
	if after := tracetest.Snapshot(t, dir); !equalSnapshots(before, after) {
		t.Errorf("the store copy's directory changed:\nbefore %v\nafter  %v", before, after)
	}
	tr, err := trace.ReadFile(tracePath)
	if err != nil {
		t.Fatal(err)
	}
	if want := (sha256.Sum256(data)); tr.Source.Digest != hexOf(want[:]) || want != digest {
		t.Errorf("the trace names digest %s", tr.Source.Digest)
	}
	var rep shadow.Report
	raw, err := os.ReadFile(reportPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &rep); err != nil || rep.Counts["same"] != 7 || rep.Trace.StoreDigest != tr.Source.Digest {
		t.Errorf("report = %v, %v", rep.Counts, err)
	}
	// the outputs are never the inputs
	if code, _, stderr := crw(t, "trace", "export", "--store-copy", copyPath, "--out", copyPath); code != 2 || !strings.Contains(stderr, "never written") {
		t.Errorf("export over the copy = %d: %s", code, stderr)
	}
	if code, _, stderr := crw(t, "measure", "--trace", tracePath, "--out", tracePath); code != 2 || !strings.Contains(stderr, "never overwritten") {
		t.Errorf("measure over the trace = %d: %s", code, stderr)
	}
	if after := tracetest.Snapshot(t, dir); !equalSnapshots(before, after) {
		t.Errorf("the store copy's directory changed after the refused runs")
	}
}

func equalSnapshots(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

func hexOf(b []byte) string {
	const digits = "0123456789abcdef"
	out := make([]byte, 0, len(b)*2)
	for _, c := range b {
		out = append(out, digits[c>>4], digits[c&15])
	}
	return string(out)
}

// TestShadowRefusesLiveStore gives all three commands the live store, a hard link to it and a symbolic link to it, and an
// output path in the live state directory: each is refused with status 2 before anything is read or written, the live file is
// unchanged and no output exists.
func TestShadowRefusesLiveStore(t *testing.T) {
	s := tracetest.NewScenario(tracetest.New(t))
	copyPath := s.Copy()
	home := os.Getenv("HOME") // the home the isolation gave this test binary
	if home == "" {
		t.Fatal("HOME is not set")
	}
	liveDir := filepath.Join(home, ".local", "state", "codex-session-relay", "0123456789abcdef")
	if err := os.MkdirAll(liveDir, 0o700); err != nil {
		t.Fatal(err)
	}
	live := filepath.Join(liveDir, "relay.sqlite3")
	data, err := os.ReadFile(copyPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(live, data, 0o600); err != nil {
		t.Fatal(err)
	}
	liveBefore := tracetest.Snapshot(t, liveDir)
	other := t.TempDir()
	hard, soft := filepath.Join(other, "hard"), filepath.Join(other, "soft")
	if err := os.Link(live, hard); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(live, soft); err != nil {
		t.Fatal(err)
	}
	// a good trace, for the commands that take one
	good := filepath.Join(other, "trace.json")
	if code, _, stderr := crw(t, "trace", "export", "--store-copy", copyPath, "--out", good); code != 0 {
		t.Fatalf("export of the copy = %d: %s", code, stderr)
	}
	liveOut := filepath.Join(liveDir, "out.json")
	for _, p := range []string{live, hard, soft} {
		for _, args := range [][]string{
			{"trace", "export", "--store-copy", p, "--out", filepath.Join(other, "never.json")},
			{"shadow", "--trace", p, "--script", script("recorded.js")},
			{"measure", "--trace", p, "--out", filepath.Join(other, "never.json")},
			{"shadow", "--trace", good, "--script", p},
		} {
			code, stdout, stderr := crw(t, args...)
			if code != 2 || !strings.Contains(stderr, "live relay store") || stdout != "" {
				t.Errorf("%v = %d, stdout %q, stderr %q", args, code, stdout, stderr)
			}
		}
	}
	for _, args := range [][]string{
		{"trace", "export", "--store-copy", copyPath, "--out", liveOut},
		{"shadow", "--trace", good, "--script", script("recorded.js"), "--out", liveOut},
		{"measure", "--trace", good, "--out", liveOut},
	} {
		if code, _, stderr := crw(t, args...); code != 2 || !strings.Contains(stderr, "live relay store") {
			t.Errorf("%v = %d: %s", args, code, stderr)
		}
	}
	if _, err := os.Stat(filepath.Join(other, "never.json")); err == nil {
		t.Errorf("a refused command wrote its output")
	}
	if after := tracetest.Snapshot(t, liveDir); !equalSnapshots(liveBefore, after) {
		t.Errorf("the live state directory changed: %v -> %v", liveBefore, after)
	}
	// the copy and its trace are not live
	if code, _, stderr := crw(t, "shadow", "--trace", good, "--script", script("recorded.js")); code != 0 {
		t.Errorf("shadow over a good trace = %d: %s", code, stderr)
	}
}

// TestCLIUsageAndFailures covers the command lines: a missing flag or a bad one is status 2, a script that does not load or that
// ends in script_error is status 1 (with the report printed for the second), --run limits the runs, and the report goes to stdout
// without --out.
func TestCLIUsageAndFailures(t *testing.T) {
	s := tracetest.NewScenario(tracetest.New(t))
	copyPath := s.Copy()
	tracePath := filepath.Join(t.TempDir(), "trace.json")
	if code, _, stderr := crw(t, "trace", "export", "--store-copy", copyPath, "--run", s.Run, "--out", tracePath); code != 0 {
		t.Fatalf("export = %d: %s", code, stderr)
	}
	for _, args := range [][]string{
		{"trace"}, {"trace", "import"}, {"trace", "export"}, {"trace", "export", "--store-copy", copyPath},
		{"shadow", "--trace", tracePath}, {"shadow", "--script", script("recorded.js")}, {"shadow", "--trace", tracePath, "--script", script("recorded.js"), "--args", "{", "--args-file", "x"},
		{"shadow", "--trace", tracePath, "--script", script("recorded.js"), "--args", "{"}, {"shadow", "--trace", tracePath, "--script", script("recorded.js"), "stray"},
		{"shadow", "--trace", tracePath, "--script", script("recorded.js"), "--run", "run-none"},
		{"measure", "--trace", tracePath}, {"measure", "--out", filepath.Join(t.TempDir(), "m.json")}, {"measure", "--trace", filepath.Join(t.TempDir(), "absent.json"), "--out", filepath.Join(t.TempDir(), "m.json")},
		{"trace", "export", "--store-copy", copyPath, "--run", "run-none", "--out", filepath.Join(t.TempDir(), "t.json")},
		{"trace", "export", "--store-copy", filepath.Join(t.TempDir(), "absent.sqlite3"), "--out", filepath.Join(t.TempDir(), "t.json")},
	} {
		if code, _, stderr := crw(t, args...); code != 2 || stderr == "" {
			t.Errorf("%v = %d, stderr %q, want 2 with a message", args, code, stderr)
		}
	}
	code, stdout, stderr := crw(t, "shadow", "--trace", tracePath, "--script", script("extra.js"))
	var rep shadow.Report
	if code != 0 || json.Unmarshal([]byte(stdout), &rep) != nil || rep.Counts["extra_spawn"] != 1 || len(rep.Runs) != 1 {
		t.Errorf("shadow extra.js = %d, %q, %q", code, stderr, stdout)
	}
	if code, _, stderr := crw(t, "shadow", "--trace", tracePath, "--script", script("syntax-error.js")); code != 1 || !strings.Contains(stderr, "does not load") {
		t.Errorf("shadow syntax-error.js = %d: %s", code, stderr)
	}
	code, stdout, _ = crw(t, "shadow", "--trace", tracePath, "--script", script("throws.js"), "--args", "null")
	if code != 1 || !strings.Contains(stdout, `"status": "script_error"`) {
		t.Errorf("shadow throws.js = %d, %q", code, stdout)
	}
	if code, out, _ := crw(t, "measure", "--trace", tracePath, "--out", filepath.Join(t.TempDir(), "m.json"), "--run", s.Run, "--oversized-tokens", "1500"); code != 0 || !strings.HasPrefix(out, "wrote ") {
		t.Errorf("measure = %d, %q", code, out)
	}
	if code, _, stderr := crw(t, "measure", "--trace", tracePath, "--out", filepath.Join(t.TempDir(), "m.json"), "--oversized-tokens", "-1"); code != 2 {
		t.Errorf("measure --oversized-tokens -1 = %d: %s", code, stderr)
	}
	if !shadow.Handles([]string{"measure"}) || shadow.Handles([]string{"run"}) || shadow.Handles(nil) {
		t.Errorf("Handles")
	}
}
