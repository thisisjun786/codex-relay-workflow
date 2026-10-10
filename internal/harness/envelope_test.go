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

// oversizedWant maps recording slugs to the explicit registration policy (CRW-1101).
func oversizedWant() map[string]string {
	return map[string]string{
		"session-start": "", "session-start-permission-advisory": "", "user-prompt-submit": "", "stop": block,
		"pre-tool-use": deny, "permission-request": "", "post-tool-use": "", "subagent-stop": block, "subagent-stop-review": "",
		"post-compact": "", "pre-tool-use-edit": "", "post-tool-use-render-observation": "", "worktree-guard": "",
		"worktree-guard-pretool": deny, "pre-tool-use-memory-write": deny, "pre-tool-use-automation-ownership": deny,
	}
}

func TestInputFailurePolicyMatchesLegSemantics(t *testing.T) {
	for _, l := range Legs() {
		if l.Event == "pre-tool-use" && (l.Stage == Guard || l.Stage == FailClosed) && l.InputFailure != InputDeny {
			t.Errorf("%s lacks guard ingress deny", l.ID)
		}
		if got := l.InputFailureOutput(Input{Overflow: true}); got != oversizedWant()[l.Slug] {
			t.Errorf("%s: %q", l.ID, got)
		}
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
		"a failing read":          {broken, "", false}, // what was read is dropped: the input reads as empty
	} {
		if raw, overflow := ReadStdin(c.in); raw != c.raw || overflow != c.overflow {
			t.Errorf("%s: %d bytes, overflow %v", name, len(raw), overflow)
		}
	}
}

// Expected values are what Node 24 prints for Buffer.from(bytes).toString("utf8"): one U+FFFD for each
// maximal invalid subpart, which the WHATWG decoder defines.
func TestReadStdinDecodesAsNodeDoes(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{"\xe2\x82\x41", "\U0000FFFDA"},
		{"\xe2\x82", "\U0000FFFD"},
		{"\xff\xfe", "\U0000FFFD\U0000FFFD"},
		{"\xed\xa0\x80", "\U0000FFFD\U0000FFFD\U0000FFFD"},
		{"\xf0\x80\x80\x80", "\U0000FFFD\U0000FFFD\U0000FFFD\U0000FFFD"},
		{"\xf4\x90\x80\x80", "\U0000FFFD\U0000FFFD\U0000FFFD\U0000FFFD"},
		{"\xc0\x80", "\U0000FFFD\U0000FFFD"},
		{"\xe0\x80\x80", "\U0000FFFD\U0000FFFD\U0000FFFD"},
		{"\xf0\x9f\x98", "\U0000FFFD"},
		{"\x61\xe2\x82\xac\xe2", "a\U000020AC\U0000FFFD"},
		{"\xf0\x9f\x98\x80\x80", "\U0001F600\U0000FFFD"},
		{"\xc3\xa9", "\U000000E9"},
	} {
		if got, overflow := ReadStdin(strings.NewReader(c.in)); got != c.want || overflow {
			t.Errorf("%q: got %q, want %q", c.in, got, c.want)
		}
	}
}
