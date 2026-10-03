package harness

import (
	"io"
	"strings"
)

// MaxStdinBytes is the most a hook may send; more is refused, never cut short (cli.ts:76).
const MaxStdinBytes = 4 * 1024 * 1024

const oversizedReason = "[crw] hook input exceeded 4194304 bytes; refusing to bypass policy enforcement"

// ReadStdin reads a hook's input, at most MaxStdinBytes of it: one byte more is an overflow and the
// input is dropped, and a read that fails reads as empty input, whatever it had returned before
// (cli.ts readStdin).
func ReadStdin(in io.Reader) (raw string, overflow bool) {
	b, err := io.ReadAll(io.LimitReader(in, MaxStdinBytes+1))
	if err != nil {
		return "", false
	}
	if len(b) > MaxStdinBytes {
		return "", true
	}
	return string(b), false
}

// OversizedHookOutput is what a leg of this slug answers to an input over MaxStdinBytes: a PreToolUse
// deny for any slug that starts with pre-tool-use, a block for stop and subagent-stop, and nothing for
// the rest (cli.ts:101-117). A slug that answers nothing leaves the process to exit 1, so the guard
// whose slug does not start with pre-tool-use (worktree-guard-pretool) is not a deny.
func OversizedHookOutput(slug string) string {
	switch {
	case strings.HasPrefix(slug, "pre-tool-use"):
		return `{"hookSpecificOutput":{"hookEventName":"PreToolUse","permissionDecision":"deny","permissionDecisionReason":"` + oversizedReason + `","additionalContext":"` + oversizedReason + `"}}` + "\n"
	case slug == "stop" || slug == "subagent-stop":
		return `{"decision":"block","reason":"` + oversizedReason + `"}` + "\n"
	}
	return ""
}
