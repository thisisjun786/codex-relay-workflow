package recall

import (
	"context"
	"io"
	"strings"
	"syscall"
	"testing"
	"testing/iotest"

	"github.com/thisisjun786/codex-relay-workflow/internal/harness"
)

type countingHookInput struct {
	io.Reader
	n int
}

func (r *countingHookInput) Read(b []byte) (int, error) {
	n, err := r.Reader.Read(b)
	r.n += n
	return n, err
}

func TestRecallIngressBoundsBeforeHandler(t *testing.T) {
	for _, size := range []int{harness.MaxStdinBytes, harness.MaxStdinBytes + 1, 16 << 20} {
		prefix := `{"hook_event_name":"SessionStart","session_id":"rec-s1","unused":"`
		raw := prefix + strings.Repeat("x", size-len(prefix)-4) + "é\"}"
		in := &countingHookInput{Reader: strings.NewReader(raw)}
		var out strings.Builder
		ran := false
		env := func(k string) (string, bool) { return "", false }
		code := RunHook(context.Background(), "session-start", in, &out, env, t.TempDir(), func(_, _, _, _ string, _ RecallContextDeps) string { ran = true; return "answer" })
		if code != 0 || in.n > harness.MaxStdinBytes+1 || ran != (size == harness.MaxStdinBytes) || size > harness.MaxStdinBytes && out.Len() != 0 {
			t.Errorf("size=%d read=%d handler=%v output=%d code=%d", size, in.n, ran, out.Len(), code)
		}
	}
	var out strings.Builder
	ran := false
	in := io.MultiReader(strings.NewReader(`{"hook_event_name":"SessionStart"}`), iotest.ErrReader(syscall.EIO))
	if RunHook(context.Background(), "session-start", in, &out, func(string) (string, bool) { return "", false }, t.TempDir(), func(_, _, _, _ string, _ RecallContextDeps) string { ran = true; return "answer" }) != 0 || out.Len() != 0 || ran {
		t.Error("failed read reached handler")
	}
}

func TestRecallIngressFirstInterruptReturns130(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	r, w := io.Pipe()
	defer r.Close()
	defer w.Close()
	var out strings.Builder
	cancel()
	if code := RunHook(ctx, "session-start", r, &out, nil, t.TempDir(), nil); code != 130 {
		t.Fatalf("interrupt: %d", code)
	}
	// A deterministic late-read run also leaves no output or handler effect.
	if code := recallHookRun(ctx, "session-start", strings.NewReader(`{}`), &out, nil, t.TempDir(), nil); code != 130 || out.Len() != 0 {
		t.Fatal("late read after interruption")
	}
}
