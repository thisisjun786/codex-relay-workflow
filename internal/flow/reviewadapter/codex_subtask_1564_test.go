package reviewadapter

// CRW-1564: CodexSubtaskReviewProfile runs `codex review -- <prompt>` — the custom-instructions review — for a scoped
// sub-task check: codex-cli 0.163.0 refuses a PROMPT together with --uncommitted or --base, so the prompt itself states the
// scope and nothing else is given. The model, review_model, effort and read-only sandbox overrides, the answer reader and
// the PromptSent record are the ordinary review profile's.

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// fakeCodexArgv writes a `codex` executable into a new bin dir, puts it first on PATH and returns the file the fake records
// its argv into, NUL-separated.
func fakeCodexArgv(t *testing.T) string {
	t.Helper()
	bin := t.TempDir()
	captured := filepath.Join(t.TempDir(), "argv")
	fake := "#!/bin/sh\n: > \"" + captured + "\"\nfirst=1\nfor a in \"$@\"; do\n" +
		"\tif [ $first -eq 1 ]; then first=0; else printf '\\0' >> \"" + captured + "\"; fi\n\tprintf '%s' \"$a\" >> \"" + captured + "\"\ndone\n" +
		"cat <<'CRWEOF'\n" + codexClean + "CRWEOF\n"
	if err := os.WriteFile(filepath.Join(bin, "codex"), []byte(fake), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	return captured
}

// TestCodexSubtaskProfileRunsReviewWithThePrompt: the real ExecRun process boundary gets the scope prompt as exactly one
// argv element after `--`, with no --uncommitted or --base anywhere, and the record says the prompt was sent.
func TestCodexSubtaskProfileRunsReviewWithThePrompt(t *testing.T) {
	captured := fakeCodexArgv(t)
	const prompt = "Review the uncommitted changes in this working tree.\nSUB-TASK 1 of 2 (one): write one\nDONE CONDITION: a/one.txt holds one\nEDIT REGIONS: a/one.txt\nNOT DUE YET: two\n"
	p := CodexSubtaskReviewProfile(CodexReviewConfig{Model: "gpt-6.1-sol", Effort: "xhigh"})
	res, err := Adapter{Run: ExecRun, Fingerprint: fixedPrint}.Check(t.Context(), Request{Profile: p, Worktree: t.TempDir(), Prompt: prompt,
		Schema: AnswerSchema, Limit: time.Minute, GuideRev: "crw-unit-check/2"})
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	raw, err := os.ReadFile(captured)
	if err != nil {
		t.Fatalf("the fake codex wrote no argv: %v", err)
	}
	argv := strings.Split(string(raw), "\x00")
	if argv[0] != "review" {
		t.Fatalf("argv %v is no `codex review`", argv)
	}
	// the prompt is one argv element, after --; no diff-scope flag is anywhere
	at := slices.Index(argv, "--")
	if at < 0 || at+1 >= len(argv) || argv[at+1] != prompt {
		t.Fatalf("the scope prompt is not the one element after --: %v", argv)
	}
	for _, a := range argv {
		if a == "--uncommitted" || a == "--base" || strings.HasPrefix(a, "--base=") {
			t.Fatalf("a diff-scope flag reached the tool: %v", argv)
		}
	}
	for _, want := range []string{`model="gpt-6.1-sol"`, `review_model="gpt-6.1-sol"`, `model_reasoning_effort="xhigh"`, `sandbox_mode="read-only"`} {
		if !slices.Contains(argv, want) {
			t.Errorf("argv %v lacks %s", argv, want)
		}
	}
	if !res.PromptSent {
		t.Error("the record does not say the prompt was sent")
	}
	sum := sha256.Sum256([]byte(prompt))
	if res.PromptDigest != hex.EncodeToString(sum[:]) {
		t.Errorf("prompt digest %q, want sha256 of the scope prompt", res.PromptDigest)
	}
	if res.State != StateOK || res.Model != "gpt-6.1-sol" || res.Effort != "xhigh" {
		t.Errorf("the answer's record %+v", res)
	}
}

// TestCodexSubtaskProfileRefusesWithoutItsScope: a sub-task review takes only its prompt-scoped uncommitted change: a blank
// prompt and any range (Base or Head) are refused before a call.
func TestCodexSubtaskProfileRefusesWithoutItsScope(t *testing.T) {
	p := CodexSubtaskReviewProfile(CodexReviewConfig{Model: "gpt-6.1-sol", Effort: "xhigh"})
	for name, req := range map[string]Request{
		"no prompt":  {},
		"base set":   {Prompt: "p", Base: "abc"},
		"head set":   {Prompt: "p", Head: "def"},
		"base+head":  {Prompt: "p", Base: "abc", Head: "def"},
		"space only": {Prompt: "   "},
	} {
		if _, err := p.Args(req); err == nil {
			t.Errorf("%s: the profile accepted the request", name)
		}
	}
	// a refused Args is a not-started error, no call
	res, err := Adapter{Run: ExecRun, Fingerprint: fixedPrint}.Check(t.Context(), Request{Profile: p, Worktree: t.TempDir(), Prompt: "p", Base: "abc",
		Schema: AnswerSchema, Limit: time.Minute, GuideRev: "crw-unit-check/2"})
	if err == nil || !strings.Contains(err.Error(), "uncommitted change") {
		t.Fatalf("a ranged sub-task review ran: %v (%+v)", err, res)
	}
}
