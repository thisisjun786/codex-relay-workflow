package agy

import (
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

// TestClassify drives the real runner against the fake agy: every row is one call and the class and reason it must get. Texts marked R0 are agy 1.2.16's own output
// as measured (testdata); the authentication text is the wording agy's headless documentation gives, which R0 did not see fail.
func TestClassify(t *testing.T) {
	const prompt = "review this diff"
	nonASCII := "héllo 😀"
	ok := `{"status":"SUCCESS","response":"Found one issue.","usage":{"total_tokens":10}}`
	for _, c := range []struct {
		name   string
		spec   fakeSpec
		prompt string
		schema bool
		class  Class
		reason Reason
		detail string
	}{
		{"normal with a schema", fakeSpec{Stdout: testdata(t, "success_schema.json")}, prompt, true, ClassNormal, "", ""},
		{"normal without a schema", fakeSpec{Stdout: ok}, prompt, false, ClassNormal, "", ""},
		{"normal, non-ASCII prompt counted in bytes", fakeSpec{Stdout: ok}, nonASCII, false, ClassNormal, "", ""},

		{"invalid: denied actions (R0)", fakeSpec{Stdout: testdata(t, "denied_actions.json")}, prompt, true, ClassInvalid, ReasonDeniedActions, "RunCommand"},
		{"invalid: empty response", fakeSpec{Stdout: `{"status":"SUCCESS","response":"  \n"}`}, prompt, false, ClassInvalid, ReasonEmptyResponse, ""},
		{"invalid: schema but no structured output", fakeSpec{Stdout: ok}, prompt, true, ClassInvalid, ReasonNoStructured, ""},
		{"invalid: structured output null", fakeSpec{Stdout: `{"status":"SUCCESS","response":"x","structured_output":null}`}, prompt, true, ClassInvalid, ReasonNoStructured, ""},
		{"invalid: print timeout partial (R0)", fakeSpec{Stdout: testdata(t, "print_timeout.json"), Stderr: testdata(t, "print_timeout.stderr")}, prompt, false, ClassInvalid, ReasonPartialResponse, ""},
		{"invalid: time limit", fakeSpec{Sleep: time.Minute}, prompt, false, ClassInvalid, ReasonTimeLimit, "400ms"},
		{"invalid: prompt taken as one character (the --print=- shape, R0)", fakeSpec{Stdout: `{"status":"SUCCESS","response":""}`, LogLength: 1}, prompt, false, ClassInvalid, ReasonPromptLength, "promptLength=1"},
		{"invalid: prompt length one too long", fakeSpec{Stdout: ok, LogLength: len(prompt) + 1}, prompt, false, ClassInvalid, ReasonPromptLength, ""},
		{"invalid: prompt length counted in runes", fakeSpec{Stdout: ok, LogLength: utf8.RuneCountInString(nonASCII)}, nonASCII, false, ClassInvalid, ReasonPromptLength, ""},
		{"invalid: no promptLength line in the log", fakeSpec{Stdout: ok, LogLength: -1}, prompt, false, ClassInvalid, ReasonPromptLength, "no promptLength"},

		{"unavailable: quota (R0)", fakeSpec{Exit: 3, Stderr: testdata(t, "quota.stderr")}, prompt, true, ClassUnavailable, ReasonQuota, "Resets in 3h43m23s"},
		{"unavailable: quota in the envelope", fakeSpec{Exit: 3, Stdout: `{"status":"ERROR","error":"RESOURCE_EXHAUSTED (code 429): Individual quota reached."}`}, prompt, true, ClassUnavailable, ReasonQuota, ""},
		{"unavailable: authentication", fakeSpec{Exit: 1, Stderr: "Error: authentication required: sign in with agy before a headless run\n"}, prompt, true, ClassUnavailable, ReasonAuthentication, ""},
		{"unavailable: unknown model (R0)", fakeSpec{Exit: 1, Stdout: testdata(t, "unknown_model.json")}, prompt, true, ClassUnavailable, ReasonUnknownModel, "does-not-exist-model"},
		{"unavailable: content filter (R0)", fakeSpec{Exit: 3, Stdout: testdata(t, "content_filter.json"), Stderr: testdata(t, "content_filter.stderr")}, prompt, true, ClassUnavailable, ReasonContentFilter, ""},
		{"unavailable: crash, other exit code", fakeSpec{Exit: 2, Stderr: "panic: boom\n"}, prompt, true, ClassUnavailable, ReasonCrash, "exit 2: panic: boom"},
		{"unavailable: crash, killed by a signal", fakeSpec{Kill: true}, prompt, true, ClassUnavailable, ReasonCrash, "signal"},
		{"unavailable: crash, unparseable output", fakeSpec{Stdout: "not json at all"}, prompt, true, ClassUnavailable, ReasonCrash, "envelope"},
		{"unavailable: crash, status ERROR with exit 0", fakeSpec{Stdout: `{"status":"ERROR","error":"boom"}`}, prompt, true, ClassUnavailable, ReasonCrash, "boom"},
	} {
		t.Run(c.name, func(t *testing.T) {
			cfg, _ := fakeCfg(t, c.spec)
			cfg.TimeLimitFloor, cfg.TimeLimitCeiling = 400*time.Millisecond, 400*time.Millisecond
			var schema []byte
			if c.schema {
				schema = []byte(`{"type":"object"}`)
			}
			res := run(t, cfg, Request{Prompt: []byte(c.prompt), Schema: schema})
			if res.Class != c.class || res.Reason != c.reason || !strings.Contains(res.Detail, c.detail) {
				t.Errorf("got %s/%s (%s), want %s/%s containing %q", res.Class, res.Reason, res.Detail, c.class, c.reason, c.detail)
			}
			if (res.StructuredOutput != nil) != (c.class == ClassNormal && c.schema) {
				t.Errorf("structured output %s for a %s call", res.StructuredOutput, res.Class)
			}
		})
	}
}
