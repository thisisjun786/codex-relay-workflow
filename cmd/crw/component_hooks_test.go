package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"github.com/thisisjun786/codex-relay-workflow/internal/harness"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/job"
)

func TestBgComponentHookModeRoutesTheThreeLegs(t *testing.T) {
	old := os.Stdin
	t.Cleanup(func() { os.Stdin = old })
	t.Setenv("CODEX_HOME", t.TempDir())
	t.Setenv("CRW_BGWAKE", "1")
	for _, c := range []struct{ event, id, want string }{
		{"stop", "stop-waking-on-background-completion", `"decision":"block"`},
		{"user-prompt-submit", "user-prompt-submit-delivering-background-completions", `"hookEventName":"UserPromptSubmit"`},
		{"session-start", "session-start-adopting-background-completions", `"hookEventName":"SessionStart"`},
		{"unclaimed-component-event", "no-component", ""},
	} {
		ws := t.TempDir()
		if _, err := job.EnsureDir(ws); err != nil {
			t.Fatal(err)
		}
		sid, ended, zero := "S1", "2026-09-09T00:04:12.000Z", float64(0)
		r := job.BgRecord{ID: "build1", Cwd: ws, SessionID: &sid, Command: []string{"build"}, Status: job.StatusComplete, ExitCode: &zero, StartedAt: "2026-09-09T00:00:00.000Z", EndedAt: &ended}
		if err := job.WriteRecord(ws, r); err != nil {
			t.Fatal(err)
		}
		p := filepath.Join(ws, "input")
		if err := os.WriteFile(p, []byte(`{"session_id":"S1","cwd":"`+ws+`"}`), 0o600); err != nil {
			t.Fatal(err)
		}
		f, err := os.Open(p)
		if err != nil {
			t.Fatal(err)
		}
		os.Stdin = f
		var out, errOut strings.Builder
		code := run(context.Background(), "crw", []string{"hook", c.event, "--leg", c.id}, &out, &errOut)
		f.Close()
		if code != 0 || errOut.Len() != 0 || c.want == "" && out.Len() != 0 || c.want != "" && !strings.Contains(out.String(), c.want) {
			t.Fatalf("%s: %d %q %q", c.id, code, out.String(), errOut.String())
		}
		back, _ := job.ReadRecord(ws, "build1")
		if (back.DeliveredAt != nil) != (c.event == "stop" || c.event == "user-prompt-submit") {
			t.Fatal("unexpected delivery")
		}
	}
}

type unreadComponentInput struct{}

func (unreadComponentInput) Read([]byte) (int, error) { panic("unclaimed input read") }

func TestComponentHookTableExtendsWithoutChangingDispatch(t *testing.T) {
	ran := false
	rows := append(componentHooks(), componentHook{"future-component", "future-event", func(c invocation, in io.Reader) int { ran = true; return 7 }})
	for _, args := range [][]string{{"future-event", "--leg", "future-component"}, {"future-event", "--leg=future-component"}} {
		ran = false
		claimed, code := runComponentHook(invocation{ctx: context.Background(), args: args}, unreadComponentInput{}, rows)
		if !claimed || code != 7 || !ran {
			t.Fatal("row extension not routed")
		}
	}
	for _, args := range [][]string{nil, {"future-event"}, {"wrong", "--leg", "future-component"}, {"future-event", "--leg", "wrong"}, {"future-event", "--leg=future-component", "extra"}} {
		ran = false
		claimed, _ := runComponentHook(invocation{args: args}, unreadComponentInput{}, rows)
		if claimed || ran {
			t.Fatalf("claimed %v", args)
		}
	}
}

func TestFallbackComponentHookRoutesSessionStart(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("CODEX_HOME", t.TempDir())
	t.Setenv("CRW_HOME", t.TempDir())
	t.Setenv("CODEX_MODELS_CACHE_PATH", filepath.Join(t.TempDir(), "missing.json"))
	for _, args := range [][]string{{"session-start", "--leg", "session-start-announcing-subagent-fallback"}, {"session-start", "--leg=session-start-announcing-subagent-fallback"}} {
		var out strings.Builder
		claimed, code := runComponentHook(invocation{ctx: context.Background(), args: args, stdout: &out}, strings.NewReader(`{}`), componentHooks())
		if !claimed || code != 0 || !strings.Contains(out.String(), `"hookEventName":"SessionStart"`) {
			t.Fatalf("claimed=%v code=%d output=%q", claimed, code, out.String())
		}
	}
}

func fallbackComponentFixture(t *testing.T, name string, target any) {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "internal", "role", "testdata", "fallback", name))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, target); err != nil {
		t.Fatal(err)
	}
}

func fallbackComponentEnv(t *testing.T) map[string]string {
	t.Helper()
	root := t.TempDir()
	m := map[string]string{"HOME": filepath.Join(root, "home"), "CODEX_HOME": filepath.Join(root, "codex"), "CRW_HOME": filepath.Join(root, "global"), "CODEX_MODELS_CACHE_PATH": filepath.Join(root, "absent.json"), "PLUGIN_ROOT": ""}
	for k, v := range m {
		t.Setenv(k, v)
	}
	for _, k := range []string{"HOME", "CODEX_HOME", "CRW_HOME"} {
		if err := os.MkdirAll(m[k], 0700); err != nil {
			t.Fatal(err)
		}
	}
	return m
}

func fallbackComponentHook(ctx context.Context, in io.Reader, out io.Writer) int {
	claimed, code := runComponentHook(invocation{ctx: ctx, args: []string{"session-start", "--leg", "session-start-announcing-subagent-fallback"}, stdout: out}, in, componentHooks())
	if !claimed {
		return -2
	}
	return code
}
func TestFallbackHookRecordedRawInputs(t *testing.T) {
	var cases []struct {
		Name           string
		Input          json.RawMessage
		Pad, Exit      int
		Stdout, Stderr string
	}
	fallbackComponentFixture(t, "hooks.json", &cases)
	for _, c := range cases {
		t.Run(c.Name, func(t *testing.T) {
			_ = fallbackComponentEnv(t)
			var data []byte
			if len(c.Input) > 0 && c.Input[0] == '"' {
				var s string
				if err := json.Unmarshal(c.Input, &s); err != nil {
					t.Fatal(err)
				}
				if c.Pad > 0 {
					s += strings.Repeat(" ", c.Pad-len(s))
				}
				data = []byte(s)
			} else if err := json.Unmarshal(c.Input, &data); err != nil {
				t.Fatal(err)
			}
			var out bytes.Buffer
			if code := fallbackComponentHook(context.Background(), bytes.NewReader(data), &out); code != c.Exit || out.String() != c.Stdout || c.Stderr != "" {
				t.Fatalf("hook: exit=%d output=%q want=%q", code, out.String(), c.Stdout)
			}
		})
	}
}

type fallbackBrokenIO struct{}

func (fallbackBrokenIO) Read(p []byte) (int, error) {
	return copy(p, []byte(`{"session_id":"fixture-session"}`)), errors.New("read failure")
}
func (fallbackBrokenIO) Write([]byte) (int, error) { return 0, errors.New("write failure") }

type fallbackSignalReader struct {
	in                io.Reader
	started, finished chan struct{}
	once              sync.Once
}

func (r *fallbackSignalReader) Read(p []byte) (int, error) {
	r.once.Do(func() { close(r.started) })
	n, err := r.in.Read(p)
	if err != nil {
		close(r.finished)
	}
	return n, err
}

func TestFallbackHookObservationAndErrorOrder(t *testing.T) {
	for _, name := range []string{"root", "child", "overflow", "read-error", "cancelled", "cancel-in-read"} {
		t.Run(name, func(t *testing.T) {
			m := fallbackComponentEnv(t)
			plugin := filepath.Join(t.TempDir(), "plugin")
			dir := filepath.Join(plugin, ".codex-plugin")
			if err := os.MkdirAll(dir, 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "plugin.json"), []byte(`{"version":"1.0.0"}`), 0600); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PLUGIN_ROOT", plugin)
			raw := `{"session_id":"fixture-session"}`
			if name == "child" {
				raw = `{"session_id":"fixture-session","agent_id":"child"}`
			}
			if name == "overflow" {
				raw += strings.Repeat(" ", 65537-len(raw))
			}
			var in io.Reader = strings.NewReader(raw)
			if name == "read-error" {
				in = fallbackBrokenIO{}
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if name == "cancelled" {
				cancel()
			}
			var out bytes.Buffer
			code := 0
			if name == "cancel-in-read" {
				r, w := io.Pipe()
				defer r.Close()
				defer w.Close()
				sr := &fallbackSignalReader{in: r, started: make(chan struct{}), finished: make(chan struct{})}
				result := make(chan int, 1)
				go func() { result <- fallbackComponentHook(ctx, sr, &out) }()
				select {
				case <-sr.started:
				case early := <-result:
					t.Fatalf("hook returned %d without reading input", early)
				}
				cancel()
				code = <-result
				if _, err := io.WriteString(w, raw); err != nil {
					t.Fatal(err)
				}
				w.Close()
				<-sr.finished
			} else {
				code = fallbackComponentHook(ctx, in, &out)
			}
			wantCode := 0
			if strings.HasPrefix(name, "cancel") {
				wantCode = harness.Interrupted
			}
			if code != wantCode {
				t.Fatalf("exit %d want %d", code, wantCode)
			}
			var records []string
			if err := filepath.WalkDir(m["CODEX_HOME"], func(p string, d os.DirEntry, err error) error {
				if err != nil {
					return err
				}
				if !d.IsDir() && strings.HasSuffix(p, ".json") {
					records = append(records, p)
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			want := 0
			if name == "root" || name == "child" {
				want = 1
			}
			if len(records) != want {
				t.Fatalf("records %d want %d", len(records), want)
			}
			if want == 1 {
				b, err := os.ReadFile(records[0])
				if err != nil {
					t.Fatal(err)
				}
				var record map[string]any
				if json.Unmarshal(b, &record) != nil || record["component"] != "subagent-config" || record["event"] != "session-start" {
					t.Fatalf("record %s", b)
				}
				if name == "child" && record["agentId"] != "child" {
					t.Fatal("child observation missing")
				}
			}
			if (out.Len() > 0) != (name == "root") {
				t.Fatalf("output %q", out.String())
			}
		})
	}
	_ = fallbackComponentEnv(t)
	if code := fallbackComponentHook(context.Background(), strings.NewReader(`{}`), fallbackBrokenIO{}); code != 0 {
		t.Fatal("writer error visible")
	}
}
