package job

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/harness"
)

func hookDone(t *testing.T, ws, id, session string) BgRecord {
	t.Helper()
	r := finished(ws, id, "2026-09-09T00:04:12.000Z")
	r.SessionID, r.Command = sp(session), []string{"npm", "run", "build"}
	return save(t, ws, r)
}

func hookEnv(values map[string]string) func(string) string {
	return func(k string) string { return values[k] }
}

func hookJSON(t *testing.T, raw string) map[string]any {
	t.Helper()
	var v map[string]any
	if err := json.Unmarshal([]byte(raw), &v); err != nil {
		t.Fatalf("invalid hook answer %q: %v", raw, err)
	}
	return v
}

// The ten B-class scenarios are bg-wake/test/hook.test.ts:46-136.
func TestBgHookStopOnceAndPromptNeverDecides(t *testing.T) {
	for _, event := range []string{"stop", "user-prompt-submit"} {
		t.Run(event, func(t *testing.T) {
			ws := workspace(t)
			hookDone(t, ws, "build1", "S1")
			p := HookPayload{SessionID: "S1", Cwd: ws}
			run := HandleStop
			if event != "stop" {
				run = HandleUserPromptSubmit
			}
			v := hookJSON(t, run(p, ws, hookEnv(nil), noon))
			if event == "stop" {
				if v["decision"] != "block" || !strings.Contains(v["reason"].(string), "build1") {
					t.Fatalf("block: %v", v)
				}
			} else {
				o := v["hookSpecificOutput"].(map[string]any)
				if _, decides := v["decision"]; decides || o["hookEventName"] != "UserPromptSubmit" || !strings.Contains(o["additionalContext"].(string), "build1") {
					t.Fatalf("injection: %v", v)
				}
			}
			if r, _ := ReadRecord(ws, "build1"); r.DeliveredAt == nil {
				t.Fatal("completion not stamped before output")
			}
			if got := run(p, ws, hookEnv(nil), noon); got != "" {
				t.Fatalf("second wake %q", got)
			}
		})
	}
}

func TestBgHookQuietStatesAndSwitches(t *testing.T) {
	for _, kind := range []string{"running", "foreign", "corrupt", "missing", "off", "env-off"} {
		t.Run(kind, func(t *testing.T) {
			ws, env := workspace(t), hookEnv(nil)
			switch kind {
			case "running":
				r := mk(ws, "still")
				r.StartedAt = noon().Format(time.RFC3339Nano)
				save(t, ws, r)
			case "foreign":
				hookDone(t, ws, "theirs", "OTHER")
			case "corrupt":
				put(t, RecordPath(ws, "broken"), "{ not json")
			case "off", "env-off":
				hookDone(t, ws, "build1", "S1")
				if kind == "off" {
					put(t, DisabledPath(ws), "off")
				} else {
					env = hookEnv(map[string]string{EnvVar: "0"})
				}
			}
			p := HookPayload{SessionID: "S1", Cwd: ws}
			for _, run := range []func(HookPayload, string, func(string) string, func() time.Time) string{HandleStop, HandleUserPromptSubmit} {
				if got := run(p, ws, env, noon); got != "" {
					t.Fatalf("quiet %s: %q", kind, got)
				}
			}
			if kind != "running" && kind != "foreign" && HandleSessionStart(p, ws, env, noon) != "" {
				t.Fatal("quiet session-start")
			}
			if r, ok := ReadRecord(ws, "build1"); ok && r.DeliveredAt != nil {
				t.Fatal("silenced completion was consumed")
			}
		})
	}
}

func TestBgHookAdoptionAndAffordance(t *testing.T) {
	for _, off := range []bool{false, true} {
		ws := workspace(t)
		hookDone(t, ws, "old", "GONE")
		if off {
			put(t, DisabledPath(ws), "off")
		}
		out := HandleSessionStart(HookPayload{SessionID: "S2", Cwd: ws}, ws, hookEnv(nil), noon)
		r, _ := ReadRecord(ws, "old")
		if r.AdoptedBy == nil || *r.AdoptedBy != "S2" || r.DeliveredAt != nil || off && out != "" {
			t.Fatalf("adoption off=%v: %+v %q", off, r, out)
		}
		if !off {
			v := hookJSON(t, out)["hookSpecificOutput"].(map[string]any)
			if v["hookEventName"] != "SessionStart" || !strings.Contains(v["additionalContext"].(string), "old") {
				t.Fatal(v)
			}
			if HandleStop(HookPayload{SessionID: "S2"}, ws, hookEnv(nil), noon) == "" {
				t.Fatal("adopted completion did not wake")
			}
		}
	}
	ws := workspace(t)
	r := hookDone(t, ws, "done", "S1")
	r.DeliveredAt = sp("2026-09-09T00:05:00.000Z")
	save(t, ws, r)
	golden := strings.TrimSuffix(get(t, "testdata/hook/AFFORDANCE.txt"), "\n")
	out := HandleSessionStart(HookPayload{SessionID: "S1"}, ws, hookEnv(nil), noon)
	if got := hookJSON(t, out)["hookSpecificOutput"].(map[string]any)["additionalContext"]; got != golden || Affordance != golden {
		t.Fatalf("AFFORDANCE %q, want %q", got, golden)
	}
}

func TestBgHookBatchDrainAndReAdoption(t *testing.T) {
	ws := workspace(t)
	for i := range 8 {
		r := hookDone(t, ws, string(rune('a'+i)), "S1")
		r.EndedAt = sp(noon().Add(time.Duration(i) * time.Second).Format(time.RFC3339))
		save(t, ws, r)
	}
	if out := HandleStop(HookPayload{SessionID: "S1"}, ws, hookEnv(nil), noon); !strings.Contains(out, "5건") {
		t.Fatal(out)
	}
	left := 0
	for _, id := range ListRecordIDs(ws) {
		if r, _ := ReadRecord(ws, id); r.DeliveredAt == nil {
			left++
		}
	}
	put(t, DisabledPath(ws), "off")
	if left != 3 || !strings.Contains(DrainNow(ws, sp("S1"), noon), "3건") || DrainNow(ws, sp("S1"), noon) != "" {
		t.Fatal("drain did not collect remaining jobs while off")
	}
	hookDone(t, ws, "orphan", "GONE")
	for _, sid := range []string{"A", "B"} {
		HandleSessionStart(HookPayload{SessionID: sid}, ws, hookEnv(nil), noon)
	}
	if r, _ := ReadRecord(ws, "orphan"); *r.AdoptedBy != "B" || r.DeliveredAt != nil {
		t.Fatal("oracle re-adoption behavior changed")
	}
}

func TestBgHookPayloadAndRawContext(t *testing.T) {
	for _, c := range []struct{ raw, sid, cwd string }{
		{`{"session_id":" S1 ","cwd":" "}`, " S1 ", " "},
		{`{"session_id":12,"cwd":false}`, "ENV", "fallback"},
		{`{"session_id":"S1","x":1e400}`, "S1", "fallback"},
		{`{"session_id":"S1"}{}`, "ENV", "fallback"},
		{"\ufeff{}", "ENV", "fallback"}, {`[]`, "ENV", "fallback"}, {`null`, "ENV", "fallback"}, {`Infinity`, "ENV", "fallback"},
	} {
		p := parseHookPayload(c.raw)
		sid := PayloadSessionID(p, hookEnv(map[string]string{"CODEX_THREAD_ID": " \ufeffENV\t"}))
		if sid == nil || *sid != c.sid || PayloadCwd(p, "fallback") != c.cwd {
			t.Errorf("%q: %+v %v", c.raw, p, sid)
		}
	}
	if PayloadSessionID(HookPayload{}, hookEnv(nil)) != nil {
		t.Fatal("empty session")
	}
	body := " \r\n" + strings.Repeat("x", 33000) + "<&>\u2028\r\n "
	if got := hookJSON(t, contextEnvelope("SessionStart", body))["hookSpecificOutput"].(map[string]any)["additionalContext"]; got != body {
		t.Fatal("raw context normalized or truncated")
	}
}

type badHookReader struct{ panicRead bool }

func (r badHookReader) Read([]byte) (int, error) {
	if r.panicRead {
		panic("read")
	}
	return 0, errors.New("read")
}

type badHookWriter struct{ panicWrite bool }

func (w badHookWriter) Write([]byte) (int, error) {
	if w.panicWrite {
		panic("write")
	}
	return 0, errors.New("write")
}

func TestBgRunHookThresholdFallbackAndErrors(t *testing.T) {
	base := `{"session_id":"X","x":""}`
	atAstral := `{"session_id":"X","x":"` + strings.Repeat("😀", (MaxHookUnits-len(base))/2) + `"}`
	for _, c := range []struct {
		name  string
		in    io.Reader
		wakes bool
	}{
		{"at-ascii", strings.NewReader(strings.Repeat(" ", 1<<20-18) + `{"session_id":"X"}`), false},
		{"over-ascii", strings.NewReader(strings.Repeat(" ", 1<<20) + `{"session_id":"X"}`), true},
		{"bmp", strings.NewReader(`{"cwd":"` + strings.Repeat("한", 1<<19) + `","session_id":"X"}`), false},
		{"astral-over", strings.NewReader(`{"cwd":"` + strings.Repeat("😀", 1<<19) + `","session_id":"X"}`), true},
		{"astral-at", strings.NewReader(atAstral), false},
		{"harness-over", strings.NewReader(strings.Repeat("x", harness.MaxStdinBytes+1)), true},
		{"read-error", badHookReader{}, true}, {"panic-reader", badHookReader{true}, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			ws := workspace(t)
			hookDone(t, ws, "fallback", "S1")
			var out strings.Builder
			lookup := func(k string) (string, bool) { return "S1", k == "CODEX_THREAD_ID" }
			code := RunHook(context.Background(), "stop", c.in, &out, lookup, ws, noon)
			if code != 0 || (out.Len() > 0) != c.wakes {
				t.Fatalf("code=%d wake=%q expected=%v", code, out.String(), c.wakes)
			}
		})
	}
	for _, panicWrite := range []bool{false, true} {
		ws := workspace(t)
		hookDone(t, ws, "lost", "S1")
		lookup := func(k string) (string, bool) { return "S1", k == "CODEX_THREAD_ID" }
		if RunHook(context.Background(), "stop", strings.NewReader("{}"), badHookWriter{panicWrite}, lookup, ws, noon) != 0 {
			t.Fatal("broken stdout failed closed")
		}
		if r, _ := ReadRecord(ws, "lost"); r.DeliveredAt == nil {
			t.Fatal("oracle stamps even when output is lost")
		}
	}
	ws := workspace(t)
	hookDone(t, ws, "clock", "S1")
	if HandleStop(HookPayload{SessionID: "S1"}, ws, hookEnv(nil), func() time.Time { panic("clock") }) != "" || HandleSessionStart(HookPayload{}, ws, func(string) string { panic("env") }, noon) != "" {
		t.Fatal("handler panic escaped")
	}
}

// The CLI corpus has no frozen clock. This drives the same recorded exit case
// through RunHook's clock seam, without changing the recorded fixture or runtime.
func TestBgRunHookRecordedReconciliationWithDrivenClock(t *testing.T) {
	var fixture struct {
		Given struct {
			JSON  map[string]json.RawMessage `json:"json"`
			Files map[string]string          `json:"files"`
		}
		Expect struct {
			Steps []struct {
				Output map[string]any `json:"stdout_json"`
			}
		}
	}
	raw := get(t, "../../../contract/fixtures/cxc/hook__stop-waking-on-background-completion__running_record_reconciled_from_exit_file.json")
	if err := json.Unmarshal([]byte(raw), &fixture); err != nil {
		t.Fatal(err)
	}
	ws := workspace(t)
	for p, raw := range fixture.Given.JSON {
		put(t, filepath.Join(ws, ".crw", strings.TrimPrefix(p, "ws/.codexclaw/")), strings.ReplaceAll(string(raw), "${WS}", ws))
	}
	for p, body := range fixture.Given.Files {
		put(t, filepath.Join(ws, ".crw", strings.TrimPrefix(p, "ws/.codexclaw/")), body)
	}
	var out strings.Builder
	clock := func() time.Time { return time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC) }
	lookup := func(string) (string, bool) { return "", false }
	if RunHook(context.Background(), "stop", strings.NewReader(`{"session_id":"rec-s1"}`), &out, lookup, ws, clock) != 0 {
		t.Fatal("hook failed")
	}
	want := fixture.Expect.Steps[0].Output["reason"].(string)
	want = strings.ReplaceAll(strings.ReplaceAll(want, "[codexclaw bg]", "[crw bg]"), "cxc bg", "crw relay job")
	if got := hookJSON(t, out.String())["reason"]; got != want {
		t.Fatalf("recorded reason %q want %q", got, want)
	}
	if r, ok := ReadRecord(ws, "build2"); !ok || r.Status != StatusFailed || r.ExitCode == nil || *r.ExitCode != 3 || r.EndedAt == nil || *r.EndedAt != "2026-01-01T00:00:00.000Z" || r.DeliveredAt == nil {
		t.Fatalf("recorded transition: %+v", r)
	}
}

func TestBgHookRegistryWriteFailureDoesNotFailClosed(t *testing.T) {
	ws := workspace(t)
	hookDone(t, ws, "unwritable", "GONE")
	if err := os.Chmod(BGDir(ws), 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(BGDir(ws), 0o755) })
	if out := HandleSessionStart(HookPayload{SessionID: "S2"}, ws, hookEnv(nil), noon); out != "" {
		t.Fatal("failed adoption injected context")
	}
	if out := HandleStop(HookPayload{SessionID: "GONE"}, ws, hookEnv(nil), noon); !strings.Contains(out, "unwritable") {
		t.Fatal("failed stamp hid the selected completion")
	}
	if r, _ := ReadRecord(ws, "unwritable"); r.AdoptedBy != nil || r.DeliveredAt != nil {
		t.Fatal("failed write changed record")
	}
}

type heldHookInput struct{ started, release, finished chan struct{} }

func (r heldHookInput) Read([]byte) (int, error) {
	close(r.started)
	<-r.release
	close(r.finished)
	return 0, io.EOF
}

func TestBgRunHookObservationAndCancellation(t *testing.T) {
	ws, root, home := workspace(t), workspace(t), workspace(t)
	put(t, filepath.Join(root, ".codex-plugin", "plugin.json"), `{"version":"test"}`)
	lookup := func(k string) (string, bool) {
		v, ok := map[string]string{"PLUGIN_ROOT": root, "CODEX_HOME": home}[k]
		return v, ok
	}
	for _, event := range []string{"stop", "user-prompt-submit", "session-start", "unknown"} {
		var out strings.Builder
		if RunHook(context.Background(), event, strings.NewReader(`{"session_id":"S1"}`), &out, lookup, ws, noon) != 0 || out.Len() != 0 {
			t.Fatal(event)
		}
	}
	count := 0
	filepath.WalkDir(home, func(p string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() && strings.HasSuffix(p, ".json") {
			v := hookJSON(t, get(t, p))
			if v["component"] != "bg-wake" {
				t.Fatal(v)
			}
			count++
		}
		return err
	})
	if count != 4 {
		t.Fatalf("observation count %d", count)
	}
	hookDone(t, ws, "late", "S1")
	ctx, cancel := context.WithCancel(context.Background())
	in := heldHookInput{make(chan struct{}), make(chan struct{}), make(chan struct{})}
	answer := make(chan int, 1)
	go func() { answer <- RunHook(ctx, "stop", in, io.Discard, lookup, ws, noon) }()
	<-in.started
	cancel()
	if code := <-answer; code != 130 {
		t.Fatalf("interrupt %d", code)
	}
	close(in.release)
	<-in.finished
	if r, _ := ReadRecord(ws, "late"); r.DeliveredAt != nil {
		t.Fatal("late delivery")
	}
}
