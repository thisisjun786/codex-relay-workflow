package affordance

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/harness"
)

type failedReader struct{}

func (failedReader) Read([]byte) (int, error) { return 0, errors.New("read failed") }

func TestIngressReadFailureStillEmitsSessionPointers(t *testing.T) {
	var out strings.Builder
	if code := RunHook(context.Background(), "session-start", failedReader{}, &out, testEnv, t.TempDir()); code != 0 {
		t.Fatal(code)
	}
	if got := contextOf(t, out.String(), "SessionStart"); len(strings.Split(got, "\n\n")) != 6 {
		t.Fatal("fallback pointers")
	}
	for _, event := range []string{"post-compact", "user-prompt-submit"} {
		out.Reset()
		if RunHook(context.Background(), event, failedReader{}, &out, testEnv, t.TempDir()) != 0 || out.Len() != 0 {
			t.Fatal("compact read error")
		}
	}
}

func TestIngressOwnUnboundedPolicyAndObservationName(t *testing.T) {
	ws, plugin, codex := t.TempDir(), t.TempDir(), t.TempDir()
	put(t, filepath.Join(plugin, ".codex-plugin", "plugin.json"), `{"version":"1.0.0"}`)
	t.Setenv("PLUGIN_ROOT", plugin)
	t.Setenv("CODEX_HOME", codex)
	t.Setenv("CRW_BIN", "crw")
	for _, event := range []struct{ slug, name string }{{"session-start", "SessionStart"}, {"post-compact", "PostCompact"}, {"user-prompt-submit", "UserPromptSubmit"}} {
		var out strings.Builder
		raw := payload(ws, event.name, "root", nil)
		if RunHook(context.Background(), event.slug, strings.NewReader(raw), &out, os.LookupEnv, ws) != 0 {
			t.Fatal(event.slug)
		}
	}
	n := 0
	err := filepath.WalkDir(codex, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() && strings.HasSuffix(path, ".json") {
			data, e := os.ReadFile(path)
			if e != nil {
				return e
			}
			if !strings.Contains(string(data), `"component":"cxc-ops"`) {
				t.Fatal("wrong observation component")
			}
			n++
		}
		return nil
	})
	if err != nil || n != 3 {
		t.Fatal("observations", n, err)
	}
	// cxc-ops reads even above the shared observation bound. It still sees the
	// id at the end, whereas a capped stdin reader would use empty input.
	var out strings.Builder
	raw := `{"unused":"` + strings.Repeat("x", harness.MaxStdinBytes+1) + `","session_id":"oversized"}`
	if RunHook(context.Background(), "session-start", strings.NewReader(raw), &out, os.LookupEnv, ws) != 0 || !strings.Contains(out.String(), "--session oversized") {
		t.Fatal("own stdin policy capped")
	}
	if !strings.HasSuffix(out.String(), "\n") || strings.HasSuffix(out.String(), "\n\n") {
		t.Fatal("newline doubled")
	}
}

func TestCancellationReturnsAndLateReadDoesNotWrite(t *testing.T) {
	ws := t.TempDir()
	raw := payload(ws, "PostCompact", "root", nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	r, w := io.Pipe()
	var out strings.Builder
	if code := RunHook(ctx, "post-compact", r, &out, testEnv, ws); code != harness.Interrupted {
		t.Fatal(code)
	}
	// Release the wrapper's owned blocked reader, then verify the inner
	// dispatch's completion deterministically with a second late-read call.
	r.Close()
	w.Close()
	r, w = io.Pipe()
	done := make(chan int, 1)
	go func() { done <- runHook(ctx, "post-compact", r, &out, testEnv, ws) }()
	if _, err := io.WriteString(w, raw); err != nil {
		t.Fatal(err)
	}
	w.Close()
	if code := <-done; code != harness.Interrupted {
		t.Fatal(code)
	}
	r.Close()
	if out.Len() != 0 {
		t.Fatal("cancelled output")
	}
	if _, err := os.Stat(filepath.Join(ws, ".crw")); !os.IsNotExist(err) {
		t.Fatal("late read created marker")
	}
}

func TestIngressInvalidUTF8MatchesNodeOracle(t *testing.T) {
	data, err := os.ReadFile("testdata/invalid-utf8.json")
	if err != nil {
		t.Fatal(err)
	}
	var cases []struct{ Hex, Sid, Marker string }
	if err := json.Unmarshal(data, &cases); err != nil {
		t.Fatal(err)
	}
	for _, tc := range cases {
		t.Run(tc.Hex, func(t *testing.T) {
			ws := t.TempDir()
			bytes, err := hex.DecodeString(tc.Hex)
			if err != nil {
				t.Fatal(err)
			}
			raw := `{"cwd":` + fmtQuote(ws) + `,"session_id":"` + string(bytes) + `","hook_event_name":"SessionStart"}`
			var out strings.Builder
			if RunHook(context.Background(), "session-start", strings.NewReader(raw), &out, testEnv, ws) != 0 {
				t.Fatal("exit")
			}
			ctx := contextOf(t, out.String(), "SessionStart")
			if strings.Split(ctx, "\n\n")[0] != RenderSessionBinding(tc.Sid, testEnv) {
				t.Fatal("raw UTF8 binding")
			}
			raw = strings.Replace(raw, "SessionStart", "PostCompact", 1)
			if RunHook(context.Background(), "post-compact", strings.NewReader(raw), io.Discard, testEnv, ws) != 0 {
				t.Fatal("queue exit")
			}
			entries, err := os.ReadDir(filepath.Join(ws, ".crw", "affordance-recovery"))
			if err != nil || len(entries) != 1 || entries[0].Name() != tc.Marker {
				t.Fatal("Node marker identity", err)
			}
		})
	}
}

func fmtQuote(s string) string { b, _ := json.Marshal(s); return string(b) }
