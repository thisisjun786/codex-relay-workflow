package reviewadapter

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"testing"
)

// printArgs is a tool that prints each argument it is given followed by "|".
var printArgs = []string{"sh", "-c", `printf "%s|" "$@"`, "sh"}

func digest(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// TestProfileArgsReplaceThePrompt (CRW-1517): a profile's Args are the arguments the runner puts after the argv in place of the
// prompt (nil Args: the prompt, as before), and the record's prompt digest and PromptSent name the bytes the tool was given: the
// prompt when the arguments hold it, else none (the empty string's digest). An Args that errs refuses the request before any call.
func TestProfileArgsReplaceThePrompt(t *testing.T) {
	ranged := func(req Request) ([]string, error) { return []string{"--base", req.Base, "P"}, nil }
	p := JSONProfile("args", printArgs, "grok-4.7-medium", "medium")
	p.Args = ranged
	stdout, _, exit, err := ExecRun(t.Context(), p, Request{Worktree: t.TempDir(), Prompt: "the prompt", Base: "B"})
	if err != nil || exit != 0 || string(stdout) != "--base|B|P|" {
		t.Fatalf("ExecRun with Args = %q, exit %d, err %v; want %q", stdout, exit, err, "--base|B|P|")
	}
	p.Args = nil
	stdout, _, exit, err = ExecRun(t.Context(), p, Request{Worktree: t.TempDir(), Prompt: "the prompt"})
	if err != nil || exit != 0 || string(stdout) != "the prompt|" {
		t.Fatalf("ExecRun without Args = %q, exit %d, err %v; want %q", stdout, exit, err, "the prompt|")
	}
	p.Args = func(Request) ([]string, error) { return nil, errors.New("no base") }
	if _, _, _, err := ExecRun(t.Context(), p, Request{Worktree: t.TempDir(), Prompt: "the prompt"}); !errors.Is(err, ErrNotStarted) || !strings.Contains(err.Error(), "no base") {
		t.Fatalf("ExecRun with an Args that errs: err %v; want ErrNotStarted naming the Args error", err)
	}

	answer := `{"verdict":"verified","findings":[]}`
	for _, tc := range []struct {
		name   string
		args   func(Request) ([]string, error)
		sent   bool
		digest func(prompt string) string
	}{
		{"nil Args sends the prompt", nil, true, digest},
		{"Args that keep the prompt", func(req Request) ([]string, error) { return []string{"--flag", req.Prompt}, nil }, true, digest},
		{"Args that drop the prompt", func(req Request) ([]string, error) { return []string{"--uncommitted"}, nil }, false, func(string) string { return digest("") }},
		{"Args that give nothing", func(req Request) ([]string, error) { return nil, nil }, false, func(string) string { return digest("") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := jsonProfile()
			p.Args = tc.args
			req := request(t, p)
			res, err := Adapter{Run: fakeRun{stdout: answer}.run, Fingerprint: hashFiles}.Check(t.Context(), req)
			if err != nil {
				t.Fatalf("Check: %v", err)
			}
			if res.State != StateOK || res.PromptSent != tc.sent || res.PromptDigest != tc.digest(req.Prompt) {
				t.Fatalf("Check = state %s, prompt_sent %v, digest %s; want ok, %v, %s", res.State, res.PromptSent, res.PromptDigest, tc.sent, tc.digest(req.Prompt))
			}
		})
	}

	p = jsonProfile()
	p.Args = func(Request) ([]string, error) { return nil, errors.New("the request names no base") }
	ran := false
	run := func(context.Context, Profile, Request) ([]byte, []byte, int, error) {
		ran = true
		return []byte(answer), nil, 0, nil
	}
	if res, err := (Adapter{Run: run, Fingerprint: hashFiles}).Check(t.Context(), request(t, p)); err == nil || !strings.Contains(err.Error(), "the request names no base") || ran {
		t.Fatalf("Check with an Args that errs = %+v, err %v, ran %v; want a Go error naming the Args error and no call", res, err, ran)
	}
}

// TestAnswerInGetsTheWorktree (CRW-1517): the answer reader is given the request, so a reader of a tool that prints absolute paths
// under the worktree (Codex review) can make them relative to req.Worktree; a profile with no reader is refused before any call.
func TestAnswerInGetsTheWorktree(t *testing.T) {
	p := jsonProfile()
	var got string
	p.Answer = func(stdout []byte, req Request) ([]byte, error) {
		got = req.Worktree
		return stdout, nil
	}
	req := request(t, p)
	res, err := Adapter{Run: fakeRun{stdout: `{"verdict":"verified","findings":[]}`}.run, Fingerprint: hashFiles}.Check(t.Context(), req)
	if err != nil || res.State != StateOK {
		t.Fatalf("Check = %+v, err %v; want ok", res, err)
	}
	if got != req.Worktree {
		t.Fatalf("the reader was given worktree %q; want %q", got, req.Worktree)
	}

	p.Answer = nil
	ran := false
	run := func(context.Context, Profile, Request) ([]byte, []byte, int, error) {
		ran = true
		return nil, nil, 0, nil
	}
	if _, err := (Adapter{Run: run, Fingerprint: hashFiles}).Check(t.Context(), request(t, p)); err == nil || !strings.Contains(err.Error(), "no answer reader") || ran {
		t.Fatalf("Check of a profile with no reader: err %v, ran %v; want a refusal naming the reader and no call", err, ran)
	}
}
