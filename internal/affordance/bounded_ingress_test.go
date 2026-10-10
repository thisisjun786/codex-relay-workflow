package affordance

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"testing/iotest"
	"unicode/utf16"

	"github.com/thisisjun786/codex-relay-workflow/internal/harness"
)

type countingInput struct {
	io.Reader
	n int
}

func (r *countingInput) Read(b []byte) (int, error) {
	n, err := r.Reader.Read(b)
	r.n += n
	return n, err
}

func TestAdvisoryIngressBoundsConsumptionBeforeMarkers(t *testing.T) {
	for _, size := range []int{harness.MaxStdinBytes, harness.MaxStdinBytes + 1, 16 << 20} {
		for _, event := range []string{"session-start", "post-compact"} {
			ws := t.TempDir()
			name := map[string]string{"session-start": "SessionStart", "post-compact": "PostCompact"}[event]
			prefix := `{"hook_event_name":"` + name + `","session_id":"rec-s1","cwd":` + fmtQuote(ws) + `,"unused":"`
			raw := prefix + strings.Repeat("x", size-len(prefix)-4) + "é\"}"
			in := &countingInput{Reader: strings.NewReader(raw)}
			var out strings.Builder
			code := RunHook(context.Background(), event, in, &out, testEnv, ws)
			if code != 0 || in.n > harness.MaxStdinBytes+1 {
				t.Errorf("size=%d event=%s code=%d read=%d", size, event, code, in.n)
			}
			if size > harness.MaxStdinBytes {
				if out.Len() != 0 {
					t.Error("oversized advisory output")
				}
				if _, err := os.Stat(filepath.Join(ws, ".crw")); !os.IsNotExist(err) {
					t.Error("oversized marker effect")
				}
			} else if event == "session-start" && !strings.Contains(out.String(), "--session rec-s1") {
				t.Error("boundary binding lost")
			}
		}
	}
	ws := t.TempDir()
	in := io.MultiReader(strings.NewReader(payload(ws, "SessionStart", "rec-s1", nil)), iotest.ErrReader(syscall.EIO))
	var out strings.Builder
	if RunHook(context.Background(), "session-start", in, &out, testEnv, ws) != 0 || out.Len() != 0 {
		t.Error("failed prefix ran handler")
	}
}

func TestSessionBindingRejectsUntrustedIdentity(t *testing.T) {
	ws := t.TempDir()
	for _, sid := range []string{"rec-s1", "01a122a9-88bc-7ea1-afb9-86bda4edba9f", "bad\nid", "bad`id", strings.Repeat("a", 1000000)} {
		out := RunMapAffordanceSessionStart(payload(ws, "SessionStart", sid, nil), ws, testEnv)
		ctx := contextOf(t, out, "SessionStart")
		valid := sid == "rec-s1" || strings.HasPrefix(sid, "01a122a9-")
		if len(utf16.Encode([]rune(ctx))) > harness.MaxContext || strings.Contains(ctx, "This session's id") != valid {
			t.Errorf("id length=%d valid=%v context units=%d", len(sid), valid, len(utf16.Encode([]rune(ctx))))
		}
		if valid && !strings.Contains(ctx, "--session "+sid) {
			t.Error("valid binding altered")
		}
	}
}
