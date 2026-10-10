package provider

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/harness"
	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport"
)

func TestDetectBClass(t *testing.T) {
	zero, one := 0, 1
	which := func(cmd string) string {
		if cmd != "ocx" {
			t.Fatal(cmd)
		}
		return "/fake/ocx"
	}
	for _, c := range []struct {
		name         string
		deps         Deps
		mode, reason string
	}{
		{"absent", Deps{}, "native", "ocx not found on PATH; using native Codex catalog"},
		{"no-reader", Deps{Which: which}, "error", "ocx detected but no status reader available"},
		{"nonzero", Deps{Which: which, RunStatus: func(string) (*int, string, error) { return &one, "", nil }}, "error", "ocx status exited 1"},
		{"null-exit", Deps{Which: which, RunStatus: func(string) (*int, string, error) { return nil, "", nil }}, "error", "ocx status exited null"},
		{"throws", Deps{Which: which, RunStatus: func(string) (*int, string, error) { return nil, "", errors.New("spawn EACCES") }}, "error", "ocx status invocation threw: spawn EACCES"},
		{"empty", Deps{Which: which, RunStatus: func(string) (*int, string, error) { return &zero, "  ", nil }}, "error", "ocx status produced no parseable payload"},
	} {
		t.Run(c.name, func(t *testing.T) {
			s := Detect(c.deps)
			if s.Mode != c.mode || s.Reason != c.reason {
				t.Fatalf("%+v", s)
			}
			if !strings.Contains(Line(s), c.reason) {
				t.Fatal(Line(s))
			}
		})
	}
}

// recordedPortChanges are the recorded inputs whose line the port changed on purpose (CRW-1098): the oracle advertised
// any number as the listen port.
var recordedPortChanges = map[string]string{
	"{\"proxy\":{\"running\":true},\"listen\":{\"port\":1e400}}":                                  "{\"provider\":\"ocx\",\"mode\":\"error\",\"ocxPath\":\"/fake/ocx\",\"reason\":\"ocx status reported a listen.port that is not a TCP port: 1e400\"}",
	"{\"proxy\":{\"running\":true},\"listen\":{\"port\":-0}}":                                     "{\"provider\":\"ocx\",\"mode\":\"error\",\"ocxPath\":\"/fake/ocx\",\"reason\":\"ocx status reported a listen.port that is not a TCP port: -0\"}",
	"{\"proxy\":{\"running\":true},\"defaultProvider\":\"<>&\\u2028\",\"listen\":{\"port\":1.5}}": "{\"provider\":\"ocx\",\"mode\":\"error\",\"ocxPath\":\"/fake/ocx\",\"reason\":\"ocx status reported a listen.port that is not a TCP port: 1.5\"}",
}

func TestDetectNodeRecordedStatusCases(t *testing.T) {
	f, err := os.Open("testdata/oracle.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	scan := bufio.NewScanner(f)
	n := 0
	for scan.Scan() {
		var row struct{ Input, Line string }
		if err := json.Unmarshal(scan.Bytes(), &row); err != nil {
			t.Fatal(err)
		}
		zero := 0
		s := Detect(Deps{Which: func(string) string { return "/fake/ocx" }, RunStatus: func(string) (*int, string, error) { return &zero, row.Input, nil }})
		if changed, ok := recordedPortChanges[row.Input]; ok {
			row.Line = changed
		}
		if got := Line(s); got != row.Line {
			t.Errorf("%s: %q != %q", row.Input, got, row.Line)
		}
		n++
	}
	if scan.Err() != nil || n != 10 {
		t.Fatalf("oracle rows %d: %v", n, scan.Err())
	}
	for _, raw := range []string{"{\"proxy\":{\"running\":true}}{}", "\ufeff{\"proxy\":{\"running\":true}}", "{\"proxy\":null}", "{\"proxy\":[]}", "true"} {
		// A BOM is trimmed by JS before parsing; all other cases are malformed.
		_, ok := parseStatus(raw)
		if ok != (strings.HasPrefix(raw, "\ufeff")) {
			t.Errorf("parse %q: %v", raw, ok)
		}
	}
}

func providerPath(t *testing.T, body string) string {
	t.Helper()
	root := t.TempDir()
	t.Setenv("PATH", root)
	t.Setenv("HOME", root)
	t.Setenv("CODEX_HOME", root)
	path := filepath.Join(root, "ocx")
	syscall.ForkLock.RLock()
	writeErr := os.WriteFile(path, []byte("#!/bin/sh\n"+body+"\n"), 0o755)
	syscall.ForkLock.RUnlock()
	if writeErr != nil {
		t.Fatal(writeErr)
	}
	return path
}

func TestRealCLIWithTemporaryPATH(t *testing.T) {
	for _, c := range []struct{ body, mode, reason string }{
		{"printf '%s\\n' '{\"proxy\":{\"running\":false}}'", "provider", ""},
		{"printf 'not JSON'; printf 'hidden stderr' >&2", "error", "no parseable payload"},
		{"printf 'hidden stderr' >&2; exit 3", "error", "exited 3"},
		{"kill -TERM $$", "error", "exited null"},
	} {
		providerPath(t, "test \"$*\" = 'status --json' || exit 9\n"+c.body)
		var out bytes.Buffer
		if Run(context.Background(), nil, &out, &bytes.Buffer{}) != 0 || !strings.Contains(out.String(), "\"mode\":\""+c.mode+"\"") || !strings.Contains(out.String(), c.reason) {
			t.Fatal(out.String())
		}
	}
	t.Setenv("PATH", t.TempDir())
	var out bytes.Buffer
	if Run(context.Background(), nil, &out, &bytes.Buffer{}) != 0 || !strings.Contains(out.String(), "\"mode\":\"native\"") {
		t.Fatal(out.String())
	}
}

func TestRealResolverRetainsRelativePATH(t *testing.T) {
	root := t.TempDir()
	t.Setenv("HOME", root)
	t.Setenv("CODEX_HOME", root)
	t.Chdir(root)
	if err := os.Mkdir("bin", 0o755); err != nil {
		t.Fatal(err)
	}
	if err := testsupport.WriteProgram("bin/ocx", []byte("#!/bin/sh\nprintf '%s' '{\"proxy\":{\"running\":true}}'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", "bin")
	var out bytes.Buffer
	if Run(context.Background(), nil, &out, &bytes.Buffer{}) != 0 || !strings.Contains(out.String(), "\"ocxPath\":\"bin/ocx\"") || !strings.Contains(out.String(), "\"mode\":\"provider\"") {
		t.Fatal(out.String())
	}
}

func TestResolvedOcxWithEmptyPATH(t *testing.T) {
	root := t.TempDir()
	t.Chdir(root)
	t.Setenv("HOME", root)
	t.Setenv("CODEX_HOME", root)
	t.Setenv("PATH", "")
	if err := testsupport.WriteProgram("ocx", []byte("#!/bin/sh\nprintf '%s' '{\"proxy\":{\"running\":true}}'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if Run(context.Background(), nil, &out, &bytes.Buffer{}) != 0 || !strings.Contains(out.String(), "\"ocxPath\":\"ocx\"") || !strings.Contains(out.String(), "\"mode\":\"provider\"") {
		t.Fatal(out.String())
	}
}

func TestStatusTimeoutAndBufferLimit(t *testing.T) {
	path := providerPath(t, "while :; do :; done")
	code, _, err := readStatus(context.Background(), path, 20*time.Millisecond)
	if code != nil || err == nil || err.Error() != "ocx status timed out after 20ms" {
		t.Fatalf("%v %v", code, err)
	}
	path = providerPath(t, "while :; do printf 'abcdefghijklmnopqrstuvwxyz0123456789abcdefghijklmnopqrstuvwxyz0123456789'; done")
	code, _, err = readStatus(context.Background(), path, time.Second)
	if code != nil || err == nil || !strings.Contains(err.Error(), "output exceeded") {
		t.Fatalf("overflow: %v %v", code, err)
	}
}

type badInput struct{}

func (badInput) Read([]byte) (int, error) { return 0, errors.New("unreadable") }

func TestHookInputPolicyAndObservation(t *testing.T) {
	providerPath(t, "printf '%s' '{\"proxy\":{\"running\":true}}'")
	plugin := t.TempDir()
	if err := os.Mkdir(filepath.Join(plugin, ".codex-plugin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(plugin, ".codex-plugin", "plugin.json"), []byte("{\"version\":\"test\"}"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PLUGIN_ROOT", plugin)
	for _, raw := range []string{"", "not JSON", strings.Repeat("x", harness.MaxStdinBytes+1), "{\"session_id\":\"S1\"}"} {
		var out bytes.Buffer
		if RunHook(context.Background(), strings.NewReader(raw), &out, os.LookupEnv) != 0 || !strings.Contains(out.String(), "\"hookEventName\":\"SessionStart\"") {
			t.Fatal(out.String())
		}
	}
	var out bytes.Buffer
	if RunHook(context.Background(), badInput{}, &out, os.LookupEnv) != 0 || out.Len() == 0 {
		t.Fatal("read failure suppressed detect")
	}
	var files []string
	err := filepath.WalkDir(filepath.Join(os.Getenv("CODEX_HOME"), "crw", "hook-observations"), func(p string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			files = append(files, p)
		}
		return err
	})
	if err != nil || len(files) != 1 {
		t.Fatalf("observations %v %v", files, err)
	}
	record, _ := os.ReadFile(files[0])
	if !bytes.Contains(record, []byte("\"component\":\"provider-bridge\"")) {
		t.Fatal(string(record))
	}
}

// CRW-1180 (S4-F4b): the provider line is said once per generation. A resume says it, and the compact of the same turn does not repeat
// it; startup, clear, a later compact, another session's compact and a line that changed between them say it.
func TestHookCompactRightAfterAResumeDoesNotRepeatTheLine(t *testing.T) {
	path := providerPath(t, "printf '%s' '{\"proxy\":{\"running\":true}}'")
	// Each session's transcript shows its resumed turn compacting before the SessionStart hooks run, as codex 0.154.0 writes it.
	transcripts := map[string]*pairTranscript{}
	start := func(session, source string) string {
		t.Helper()
		if transcripts[session] == nil {
			transcripts[session] = newPairTranscript(t).turn("t1", true)
		}
		var out bytes.Buffer
		raw := `{"session_id":"` + session + `","source":"` + source + `","transcript_path":"` + transcripts[session].path + `"}`
		if RunHook(context.Background(), strings.NewReader(raw), &out, os.LookupEnv) != 0 {
			t.Fatal("exit")
		}
		return out.String()
	}
	line := start("s1", "startup")
	if !strings.Contains(line, `"hookEventName":"SessionStart"`) {
		t.Fatal(line)
	}
	if got := start("s1", "resume"); got != line {
		t.Fatalf("resume = %q", got)
	}
	transcripts["s1"].said()
	if got := start("s1", "compact"); got != "" {
		t.Fatalf("compact in the turn of a resume repeated the line: %q", got)
	}
	if got := start("s1", "compact"); got != line {
		t.Fatalf("the next compact = %q", got)
	}
	for _, source := range []string{"startup", "clear", "compact", ""} {
		if got := start("s2", source); got != line {
			t.Fatalf("source %q = %q", source, got)
		}
	}
	start("s3", "resume")
	start("s3", "clear")
	if got := start("s3", "compact"); got != line {
		t.Fatalf("compact after a clear = %q", got)
	}
	start("s4", "resume")
	if got := start("s5", "compact"); got != line {
		t.Fatalf("another session's compact = %q", got)
	}
	// The bridge stopping between the resume and the compact is a different line, and it is said.
	start("s6", "resume")
	transcripts["s6"].said()
	syscall.ForkLock.RLock()
	writeErr := os.WriteFile(path, []byte("#!/bin/sh\nprintf '%s' '{\"proxy\":{\"running\":false}}'\n"), 0o755)
	syscall.ForkLock.RUnlock()
	if writeErr != nil {
		t.Fatal(writeErr)
	}
	if got := start("s6", "compact"); got == "" || got == line {
		t.Fatalf("compact after the line changed = %q", got)
	}
	// Input that names no session cannot be matched to a resume.
	for _, raw := range []string{"", "not JSON", `{"source":"compact"}`, `{"session_id":"a b","source":"compact"}`} {
		var out bytes.Buffer
		RunHook(context.Background(), strings.NewReader(raw), &out, os.LookupEnv)
		if out.Len() == 0 {
			t.Fatalf("%q was silenced", raw)
		}
	}
}

func TestHookCancellationHasNoLateEffects(t *testing.T) {
	providerPath(t, "exit 0")
	in, hold, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer in.Close()
	defer hold.Close()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var out bytes.Buffer
	if RunHook(ctx, in, &out, os.LookupEnv) != 130 || out.Len() != 0 {
		t.Fatal("cancelled hook wrote output")
	}
	hold.Close()
}
