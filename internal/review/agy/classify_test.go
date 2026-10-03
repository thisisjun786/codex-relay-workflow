package agy

import (
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

// TestClassify drives the real runner against the fake agy: every row is one call and the class and reason it must get. Texts marked R0 are agy 1.2.16's own
// output as measured (testdata); the authentication text is the wording agy's headless documentation gives, which R0 did not see fail. The fake logs R0's
// start-up lines ("not logged into Antigravity", then a silent login) on every call, so no row may read them as an authentication failure.
func TestClassify(t *testing.T) {
	const prompt = "review this diff"
	nonASCII, big := "héllo 😀", strings.Repeat("x", 1<<20)
	ok := `{"status":"SUCCESS","response":"Found one issue.","usage":{"total_tokens":10}}`
	for _, c := range []struct {
		name   string
		spec   fakeSpec
		prompt string
		schema bool
		limit  time.Duration // 0: the default one minute
		class  Class
		reason Reason
		detail string
	}{
		{"normal with a schema", fakeSpec{Stdout: testdata(t, "success_schema.json")}, prompt, true, 0, ClassNormal, "", ""},
		{"normal without a schema", fakeSpec{Stdout: ok}, prompt, false, 0, ClassNormal, "", ""},
		{"normal, non-ASCII prompt counted in bytes", fakeSpec{Stdout: ok}, nonASCII, false, 0, ClassNormal, "", ""},
		{"normal, the response only talks about quota and login", fakeSpec{Stdout: `{"status":"SUCCESS","response":"authentication required? RESOURCE_EXHAUSTED?"}`}, prompt, false, 0, ClassNormal, "", ""},

		{"invalid: denied actions (R0)", fakeSpec{Stdout: testdata(t, "denied_actions.json")}, prompt, true, 0, ClassInvalid, ReasonDeniedActions, "RunCommand"},
		{"invalid: empty response", fakeSpec{Stdout: `{"status":"SUCCESS","response":"  \n"}`}, prompt, false, 0, ClassInvalid, ReasonEmptyResponse, ""},
		{"invalid: schema but no structured output", fakeSpec{Stdout: ok}, prompt, true, 0, ClassInvalid, ReasonNoStructured, ""},
		{"invalid: structured output null", fakeSpec{Stdout: `{"status":"SUCCESS","response":"x","structured_output":null}`}, prompt, true, 0, ClassInvalid, ReasonNoStructured, ""},
		{"invalid: print timeout partial (R0)", fakeSpec{Stdout: testdata(t, "print_timeout.json"), Stderr: testdata(t, "print_timeout.stderr")}, prompt, false, 0, ClassInvalid, ReasonPartialResponse, ""},
		{"invalid: time limit", fakeSpec{Sleep: time.Minute}, prompt, false, 1500 * time.Millisecond, ClassInvalid, ReasonTimeLimit, "1.5s"},
		{"invalid: prompt taken as one character (the --print=- shape, R0)", fakeSpec{Stdout: `{"status":"SUCCESS","response":""}`, LogLength: 1}, prompt, false, 0, ClassInvalid, ReasonPromptLength, "promptLength=1"},
		{"invalid: prompt length one too long", fakeSpec{Stdout: ok, LogLength: len(prompt) + 1}, prompt, false, 0, ClassInvalid, ReasonPromptLength, ""},
		{"invalid: prompt length counted in runes", fakeSpec{Stdout: ok, LogLength: utf8.RuneCountInString(nonASCII)}, nonASCII, false, 0, ClassInvalid, ReasonPromptLength, ""},
		{"invalid: no promptLength line in the log", fakeSpec{Stdout: ok, LogLength: -1}, prompt, false, 0, ClassInvalid, ReasonPromptLength, "no promptLength"},
		{"invalid: agy exits without reading a 1 MiB prompt", fakeSpec{Stdout: ok, NoStdin: true, LogLength: -1}, big, false, 0, ClassInvalid, ReasonPromptLength, "no promptLength"},

		{"unavailable: quota (R0)", fakeSpec{Exit: 3, Stderr: testdata(t, "quota.stderr")}, prompt, true, 0, ClassUnavailable, ReasonQuota, "Resets in 3h43m23s"},
		{"unavailable: quota in the envelope", fakeSpec{Exit: 3, Stdout: `{"status":"ERROR","error":"RESOURCE_EXHAUSTED (code 429): Individual quota reached."}`}, prompt, true, 0, ClassUnavailable, ReasonQuota, ""},
		{"unavailable: authentication", fakeSpec{Exit: 1, Stderr: "Error: authentication required: sign in with agy before a headless run\n"}, prompt, true, 0, ClassUnavailable, ReasonAuthentication, ""},
		{"unavailable: unknown model (R0)", fakeSpec{Exit: 1, Stdout: testdata(t, "unknown_model.json")}, prompt, true, 0, ClassUnavailable, ReasonUnknownModel, "does-not-exist-model"},
		{"unavailable: content filter (R0)", fakeSpec{Exit: 3, Stdout: testdata(t, "content_filter.json"), Stderr: testdata(t, "content_filter.stderr")}, prompt, true, 0, ClassUnavailable, ReasonContentFilter, ""},
		{"unavailable: crash, other exit code", fakeSpec{Exit: 2, Stderr: "panic: boom\n"}, prompt, true, 0, ClassUnavailable, ReasonCrash, "exit 2: panic: boom"},
		{"unavailable: crash, a loose mention of quota", fakeSpec{Exit: 2, Stderr: "panic: quota table is nil\n"}, prompt, true, 0, ClassUnavailable, ReasonCrash, "quota table"},
		{"unavailable: crash, killed by a signal", fakeSpec{Kill: true}, prompt, true, 0, ClassUnavailable, ReasonCrash, "signal"},
		{"unavailable: crash, a signal after a quota text stays a crash", fakeSpec{Kill: true, Stderr: testdata(t, "quota.stderr")}, prompt, true, 0, ClassUnavailable, ReasonCrash, "signal"},
		{"unavailable: crash, unparseable output", fakeSpec{Stdout: "not json at all"}, prompt, true, 0, ClassUnavailable, ReasonCrash, "envelope"},
		{"unavailable: crash, status ERROR with exit 0", fakeSpec{Stdout: `{"status":"ERROR","error":"boom"}`}, prompt, true, 0, ClassUnavailable, ReasonCrash, "boom"},
	} {
		t.Run(c.name, func(t *testing.T) {
			cfg, _ := fakeCfg(t, c.spec)
			if c.limit > 0 {
				cfg.TimeLimitFloor, cfg.TimeLimitCeiling = c.limit, c.limit
			}
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
