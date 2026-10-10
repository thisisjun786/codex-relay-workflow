package provider

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/guidancerecord"
	"github.com/thisisjun786/codex-relay-workflow/internal/harness"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/host"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/text"
)

// statusTimeout is the one budget of a status probe: running ocx and draining its output share it, and only the
// short statusGrace follows it. It is a variable for the tests.
var statusTimeout = 8 * time.Second

// statusGrace is how long the probe's pipes may stay open after ocx has exited or been stopped. A child that ocx
// left running with the pipes does not hold the probe longer, and is not killed to close them: it is not the probe's.
const statusGrace = 500 * time.Millisecond

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

// statusError is a failed probe with its own diagnosis; Detect puts the reason on the error line as it is.
type statusError struct{ reason string }

func (e *statusError) Error() string { return e.reason }

// readStatus runs `ocx status --json` under one budget: starting ocx, its running and the draining of its output
// all end at timeout (plus statusGrace, which closes pipes a child of ocx still holds). It kills only ocx itself.
func readStatus(parent context.Context, path string, timeout time.Duration) (*int, string, error) {
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()
	var out bytes.Buffer
	var mu sync.Mutex
	total := 0
	overflow := false
	capture := func(w io.Writer) io.Writer {
		return statusWriter(func(p []byte) (int, error) {
			mu.Lock()
			defer mu.Unlock()
			total += len(p)
			if total > statusBufferLimit {
				overflow = true
				cancel()
				return 0, io.ErrShortBuffer
			}
			return w.Write(p)
		})
	}
	cmd := exec.CommandContext(ctx, path, "status", "--json")
	// command -v already resolved this executable. Spawn that path directly;
	// a second lookup rejects relative entries or loses it when PATH is empty.
	cmd.Path, cmd.Err = path, nil
	cmd.Stdout, cmd.Stderr = capture(&out), capture(io.Discard)
	cmd.WaitDelay = statusGrace
	err := cmd.Run()
	mu.Lock()
	raw, _ := harness.ReadStdin(bytes.NewReader(out.Bytes()))
	exceeded := overflow
	mu.Unlock()
	if parent.Err() != nil {
		return nil, raw, nil // the caller was cancelled and reports it
	}
	if exceeded {
		return nil, raw, &statusError{fmt.Sprintf("ocx status output exceeded %d bytes", statusBufferLimit)}
	}
	code := -1
	if cmd.ProcessState != nil {
		code = cmd.ProcessState.ExitCode()
	}
	if code < 0 {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return nil, raw, &statusError{"ocx status timed out after " + timeout.String()}
		}
		return nil, raw, nil
	}
	if err != nil {
		var exited *exec.ExitError
		switch {
		case errors.Is(err, exec.ErrWaitDelay):
			// ocx ended by itself and a child it left behind still holds the pipes. What it printed is judged as
			// it stands: a payload that is whole is the answer, anything else is named for what it is.
			if _, ok := parseStatus(raw); !ok {
				return nil, raw, &statusError{"ocx status ended but left its output pipe open after a partial payload"}
			}
		case !errors.As(err, &exited):
			return nil, raw, nil
		}
	}
	return &code, raw, nil
}

type statusWriter func([]byte) (int, error)

func (w statusWriter) Write(p []byte) (int, error) { return w(p) }

// Help is what `crw provider --help` prints: the command only detects, it never starts or changes anything.
const Help = `usage: crw provider [detect|status]

Prints one JSON line that says how the ocx provider bridge stands: mode "native" (ocx is not on PATH), "provider"
(ocx answered status: running, defaultProvider, port) or "error" (with the reason). It runs "ocx status --json" once,
within 8 seconds, and does not activate or start any provider. Words after detect or status are ignored. Exit
status 0 whatever the line says; 2 for an unknown command (nothing was run).`

// Run is the detect-only provider command (runProvider/runBridge): "detect" and "status" are the same as no
// argument, help is answered, and an unknown command is refused before ocx is looked for.
func Run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) > 0 {
		switch args[0] {
		case "--help", "-h", "help":
			_, _ = io.WriteString(stdout, Help+"\n")
			return 0
		}
	}
	// "detect" is the verb the CXC table puts before whatever followed `provider`, which the oracle ignored, so
	// anything after detect or status is still ignored.
	if len(args) > 0 && args[0] != "detect" && args[0] != "status" {
		fmt.Fprintf(stderr, "crw provider: unknown command %q; usage: crw provider [detect|status] (--help explains)\n", strings.Join(args, " "))
		return 2
	}
	line := Line(Detect(realDeps(ctx)))
	if ctx.Err() != nil {
		return harness.Interrupted
	}
	_, _ = io.WriteString(stdout, line+"\n")
	return 0
}

// providerLeg names this hook's record of what a session was told.
const providerLeg = "provider-bridge"

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
	// CRW-1180: the compact start right after a resume that said this very line, in the same turn, adds nothing: Codex keeps the
	// resume's output after the compaction record, so saying it again stacks the line twice. The line is live state, so a resume
	// always says it.
	if guidancerecord.SilentCompact(env, raw, providerLeg, line) {
		return 0
	}
	out := "{\"hookSpecificOutput\":{\"hookEventName\":\"SessionStart\",\"additionalContext\":" + quote(line) + "}}\n"
	n, err := io.Copy(stdout, strings.NewReader(out))
	// Only a line written whole, by a hook that was not cancelled, counts as said.
	if err == nil && n == int64(len(out)) && ctx.Err() == nil {
		guidancerecord.Said(env, raw, providerLeg, line)
	}
	return 0
}
