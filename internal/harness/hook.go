package harness

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/host"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/projectcfg"
)

// find is the leg that crw hook <event> --leg <name> (or --leg=<name>) names among legs; the event
// must be the one the leg is registered for.
func find(legs []Leg, args []string) (Leg, bool) {
	var name string
	switch {
	case len(args) == 3 && args[1] == "--leg":
		name = args[2]
	case len(args) == 2 && strings.HasPrefix(args[1], "--leg="):
		name = strings.TrimPrefix(args[1], "--leg=")
	}
	for _, l := range legs {
		if name != "" && l.ID == name && l.Event == args[0] {
			return l, true
		}
	}
	return Leg{}, false
}

// hookCwd is the payload's cwd when JSON.parse reads the input as an object whose cwd is a non-empty
// string, else the process's (cli.ts:418-426). It reads the input as the oracle does, not as asObject
// does: no trimming (a BOM fails), one document, and a number no float64 holds does not cost it.
func hookCwd(raw string) string {
	if v, ok := decode(raw); ok {
		if o, _ := v.(map[string]any); o != nil {
			if cwd, _ := o["cwd"].(string); cwd != "" {
				return cwd
			}
		}
	}
	cwd, _ := os.Getwd()
	return cwd
}

// answer runs the leg's handler; with swallow an error in it is dropped (the oracle's try/catch) and the leg answers nothing.
func (l Leg) answer(c Call, swallow bool) (out string) {
	if swallow {
		defer func() {
			if recover() != nil {
				out = ""
			}
		}()
	}
	if l.Handle != nil {
		out = l.Handle(c)
	}
	return out
}

// Interrupted is the status of a hook cancelled before it answered: 128 and SIGINT, which is how a
// shell reports the oracle's Node process, ended by SIGINT's default action at any point of its run.
const Interrupted = 130

// Hook is crw hook <event> --leg <name> for the legs given, in the order of cli.ts (337-523): an input
// over the limit is answered before anything else (the oracle reads it as empty, so no record is
// made); a Permission leg answers before the record; then the record is made, under the verb the
// oracle dispatched on (cli.ts:364), and the Guard legs run, for a subagent's turn too; a subagent's
// turn then ends; the PABCD check is read and ends the Gated legs when PABCD is off; and the
// FailClosed or Generic leg answers. The exit status is 0 unless an oversize input has no answer, or
// an error no stage swallows (exit 1, "crw cli failed", as the oracle's generic handler).
//
// The first interrupt of a crw process only cancels its run (cmd/crw serve), where Node's default
// action ends the oracle at once, even while it waits for its input: so Hook answers Interrupted as
// soon as ctx is done, whatever the run is waiting for. A read that is still blocked is not stopped
// (crw exits with the status Hook returns, which ends it; a caller that outlives Hook leaves that
// goroutine to wait for its input), but what it reads afterwards is neither recorded, run nor written.
//
// Intentional change: arguments that name no leg of the table (an unknown leg, an event that is not the
// leg's, no --leg) release the hook in silence, without reading its input and without a record. The
// oracle dispatches on a verb and, for one it does not know, reads the input, records the verb and
// exits 0; crw has no verb for a registration this build lacks, and an invocation it cannot attribute
// to a pabcd-state leg is not recorded as one.
func Hook(ctx context.Context, args []string, in io.Reader, stdout, stderr io.Writer, env host.LookupEnv, legs []Leg) int {
	leg, ok := find(legs, args)
	if !ok {
		return 0
	}
	done := make(chan int, 1)
	go func() { done <- dispatch(ctx, leg, in, stdout, stderr, env) }()
	select {
	case code := <-done:
		return code
	case <-ctx.Done():
		select {
		case code := <-done: // answered at the same moment: the answer stands
			return code
		default:
			return Interrupted
		}
	}
}

// dispatch runs one leg through the stages of Hook. Input that arrives after ctx is done finds a Hook
// that has already answered Interrupted, so nothing is recorded, run or written for it.
func dispatch(ctx context.Context, leg Leg, in io.Reader, stdout, stderr io.Writer, env host.LookupEnv) (code int) {
	defer func() {
		if p := recover(); p != nil {
			fmt.Fprintf(stderr, "crw cli failed: %v\n", p)
			code = 1
		}
	}()
	raw, overflow := ReadStdin(in)
	if ctx.Err() != nil {
		return Interrupted
	}
	if overflow {
		if out := OversizedHookOutput(leg.Slug); out != "" {
			io.WriteString(stdout, out)
		} else if leg.Stage != Permission {
			return 1
		}
		return 0
	}
	call := Call{Raw: raw}
	if leg.Stage == Permission {
		io.WriteString(stdout, leg.answer(call, true))
		return 0
	}
	RecordInvocation(raw, Component, leg.Slug, env)
	if leg.Stage == Guard {
		io.WriteString(stdout, leg.answer(call, leg.Recover))
		return 0
	}
	if !leg.SubagentExempt && IsSubagentHookPayload(raw) {
		return 0
	}
	call.PabcdEnabled = projectcfg.PabcdEnabled(hookCwd(raw), func(k string) string { v, _ := env(k); return v })
	if !call.PabcdEnabled && leg.Gated {
		return 0
	}
	io.WriteString(stdout, leg.answer(call, leg.Stage != FailClosed))
	return 0
}
