package harness

import (
	"errors"
	"io"
	"strings"
	"testing"
	"testing/iotest"
)

const (
	reason = "[crw] hook input exceeded 4194304 bytes; refusing to bypass policy enforcement"
	deny   = `{"hookSpecificOutput":{"hookEventName":"PreToolUse","permissionDecision":"deny","permissionDecisionReason":"` + reason + `","additionalContext":"` + reason + `"}}` + "\n"
	block  = `{"decision":"block","reason":"` + reason + `"}` + "\n"
)

// oversizedWant is what each of the sixteen slugs answers to an input over the limit: cli.ts
// matches "pre-tool-use" as a prefix and "stop" and "subagent-stop" exactly, so the guard slugs that
// do not start with it (worktree-guard-pretool) and subagent-stop-review answer nothing.
func oversizedWant() map[string]string {
	return map[string]string{
		"session-start": "", "session-start-permission-advisory": "", "user-prompt-submit": "", "stop": block,
		"pre-tool-use": deny, "permission-request": "", "post-tool-use": "", "subagent-stop": block, "subagent-stop-review": "",
		"post-compact": "", "pre-tool-use-edit": deny, "post-tool-use-render-observation": "", "worktree-guard": "",
		"worktree-guard-pretool": "", "pre-tool-use-memory-write": deny, "pre-tool-use-automation-ownership": deny,
	}
}

func TestOversizedHookOutputBySlug(t *testing.T) {
	for slug, want := range oversizedWant() {
		if got := OversizedHookOutput(slug); got != want {
			t.Errorf("%s: got %q, want %q", slug, got, want)
		}
	}
	if got := OversizedHookOutput("unknown"); got != "" {
		t.Errorf("an unknown slug answers %q", got)
	}
}

func TestReadStdinBoundsTheInputAtTheLimit(t *testing.T) {
	broken := io.MultiReader(strings.NewReader("abc"), iotest.ErrReader(errors.New("read failed")))
	for name, c := range map[string]struct {
		in       io.Reader
		raw      string
		overflow bool
	}{
		"empty":                   {strings.NewReader(""), "", false},
		"small":                   {strings.NewReader("{}"), "{}", false},
		"exactly the limit":       {strings.NewReader(strings.Repeat("x", MaxStdinBytes)), strings.Repeat("x", MaxStdinBytes), false},
		"one byte over the limit": {strings.NewReader(strings.Repeat("x", MaxStdinBytes+1)), "", true},
		"far over the limit":      {strings.NewReader(strings.Repeat("x", 2*MaxStdinBytes)), "", true},
		"a failing read":          {broken, "", false},                                        // what was read is dropped: the input reads as empty
		"invalid UTF-8":           {strings.NewReader("a\xff\xfeb"), "a\uFFFD\uFFFDb", false}, // as Buffer.toString("utf8") has it: the replacement characters count in the string's byte length
	} {
		if raw, overflow := ReadStdin(c.in); raw != c.raw || overflow != c.overflow {
			t.Errorf("%s: %d bytes, overflow %v", name, len(raw), overflow)
		}
	}
}
