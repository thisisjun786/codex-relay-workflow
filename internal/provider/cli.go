package provider

import (
	"bytes"
	"context"
	"io"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/harness"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/host"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/text"
)

const statusTimeout = 8 * time.Second
const statusBufferLimit = 1 << 20 // spawnSync's default combined stdout/stderr limit.

func realDeps(ctx context.Context) Deps {
	return Deps{
		Which: func(_ string) string {
			out, err := exec.CommandContext(ctx, "/bin/sh", "-c", "command -v ocx").Output()
			if err != nil {
				return ""
			}
			raw, _ := harness.ReadStdin(bytes.NewReader(out))
			return text.Trim(text.SplitLines(raw)[0])
		},
		RunStatus: func(path string) (*int, string, error) { return readStatus(ctx, path, statusTimeout) },
	}
}

func readStatus(parent context.Context, path string, timeout time.Duration) (*int, string, error) {
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()
	var out bytes.Buffer
	var mu sync.Mutex
	total := 0
	capture := func(w io.Writer) io.Writer {
		return statusWriter(func(p []byte) (int, error) {
			mu.Lock()
			defer mu.Unlock()
			total += len(p)
			if total > statusBufferLimit {
				cancel()
				return 0, io.ErrShortBuffer
			}
			return w.Write(p)
		})
	}
	cmd := exec.CommandContext(ctx, path, "status", "--json")
	cmd.Stdout, cmd.Stderr = capture(&out), capture(io.Discard)
	err := cmd.Run()
	raw, _ := harness.ReadStdin(bytes.NewReader(out.Bytes()))
	if ctx.Err() != nil || cmd.ProcessState == nil || cmd.ProcessState.ExitCode() < 0 {
		return nil, raw, nil
	}
	code := cmd.ProcessState.ExitCode()
	if err != nil {
		if _, ok := err.(*exec.ExitError); !ok {
			return nil, raw, nil
		}
	}
	return &code, raw, nil
}

type statusWriter func([]byte) (int, error)

func (w statusWriter) Write(p []byte) (int, error) { return w(p) }

// Run is runProvider/runBridge: every argument is ignored and detection exits 0.
func Run(ctx context.Context, stdout io.Writer) int {
	line := Line(Detect(realDeps(ctx)))
	if ctx.Err() != nil {
		return harness.Interrupted
	}
	_, _ = io.WriteString(stdout, line+"\n")
	return 0
}

// RunHook owns this component's input and answer. Input is used only for the
// metadata observation; malformed/oversized/unreadable input still detects.
func RunHook(ctx context.Context, in io.Reader, stdout io.Writer, env host.LookupEnv) int {
	input := make(chan string, 1)
	go func() { raw, _ := harness.ReadStdin(in); input <- raw }()
	var raw string
	select {
	case raw = <-input:
	case <-ctx.Done():
		return harness.Interrupted
	}
	if ctx.Err() != nil {
		return harness.Interrupted
	}
	harness.RecordInvocation(raw, "provider-bridge", "session-start", env)
	line := Line(Detect(realDeps(ctx)))
	if ctx.Err() != nil {
		return harness.Interrupted
	}
	out := "{\"hookSpecificOutput\":{\"hookEventName\":\"SessionStart\",\"additionalContext\":" + quote(line) + "}}\n"
	_, _ = io.Copy(stdout, strings.NewReader(out))
	return 0
}
