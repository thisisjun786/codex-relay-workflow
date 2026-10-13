package harness

import (
	"io"

	"github.com/thisisjun786/codex-relay-workflow/internal/jsnode"
)

// MaxStdinBytes is the most a hook may send; more is refused, never cut short (cli.ts:76).
const MaxStdinBytes = 4 * 1024 * 1024

const oversizedReason = "[crw] hook input exceeded 4194304 bytes; refusing to bypass policy enforcement"

// Input distinguishes transport failure from successful empty or malformed input.
// Failed reads discard even a valid prefix; no caller may dispatch it.
type Input struct {
	Raw      string
	Overflow bool
	Err      error
}

func ReadInput(in io.Reader) Input {
	b, err := io.ReadAll(io.LimitReader(in, MaxStdinBytes+1))
	if len(b) > MaxStdinBytes {
		return Input{Overflow: true, Err: err}
	}
	if err != nil {
		return Input{Err: err}
	}
	return Input{Raw: jsnode.DecodeUTF8(b)}
}

func (in Input) Failed() bool { return in.Overflow || in.Err != nil }

// ReadStdin reads a hook's input, at most MaxStdinBytes of it: one byte more is an overflow and the
// input is dropped, and a read that fails reads as empty input, whatever it had returned before
// (cli.ts readStdin). The input is text as Buffer.toString("utf8") makes it (jsnode.DecodeUTF8), so a limit on
// its byte length counts each U+FFFD as three bytes.
func ReadStdin(in io.Reader) (raw string, overflow bool) {
	input := ReadInput(in)
	return input.Raw, input.Overflow
}

// InputFailureOutput uses registration policy, never a handler's recording slug. An InputRecord leg's answer is the release
// warning; dispatch also records the release.
func (l Leg) InputFailureOutput(in Input) string {
	reason := oversizedReason
	if !in.Overflow {
		reason = "[crw] hook input could not be read; refusing to bypass policy enforcement"
	}
	switch l.InputFailure {
	case InputRecord:
		if in.Overflow {
			return releaseAnswer("over 4194304 bytes")
		}
		return releaseAnswer("a read error")
	case InputBlock:
		if !in.Overflow {
			return ""
		}
		return `{"decision":"block","reason":"` + reason + `"}` + "\n"
	}
	return ""
}
