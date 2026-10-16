package reviewadapter

import (
	"bytes"
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/jsonschema-go/jsonschema"
)

// grokEnvelope is the one JSON document grok prints for a --json-schema call, trimmed from a real run
// (a recorded critic answer): its prose text (here a different, earlier verdict than the
// structured one), the run's metadata and usage, and the structured answer.
func grokEnvelope(t *testing.T, structured any, keep bool) string {
	t.Helper()
	env := map[string]any{
		"text":           `{"verdict":"verified","findings":[]}`,
		"stopReason":     "end_turn",
		"sessionId":      "01a13fa6-7a79-78e3-b445-7bb908eb005e",
		"requestId":      "2edf7671-80e8-48c8-8c1e-057fbcd05912",
		"cwd":            "/wt",
		"thought":        "Reading the plan and the changed files.",
		"usage":          map[string]any{"input_tokens": 147894, "cache_read_input_tokens": 2729216, "cache_creation_input_tokens": 0, "output_tokens": 43462, "reasoning_tokens": 38819, "total_tokens": 2920572},
		"num_turns":      31,
		"total_cost_usd": 0.65319712,
		"modelUsage":     map[string]any{"grok-4.7-build": map[string]any{"inputTokens": 147894, "cacheReadInputTokens": 2729216}},
	}
	if keep {
		env["structuredOutput"] = structured
	}
	b, err := json.Marshal(env)
	if err != nil {
		t.Fatal(err)
	}
	return string(b) + "\n"
}

// astraStream is the JSONL codex exec --json prints, trimmed from a real run
// (a recorded critic event stream): the thread start, codex's warning as an error
// item, two prose agent_messages around a command_execution, then the given last messages and turn.completed.
func astraStream(last ...string) string {
	lines := []string{
		`{"type":"thread.started","thread_id":"01a13fcf-f9f7-77f2-8ad5-fbcc490c2036"}`,
		`{"type":"item.completed","item":{"id":"item_0","type":"error","message":"Under-development features enabled: default_mode_request_user_input."}}`,
		`{"type":"turn.started"}`,
		`{"type":"item.completed","item":{"id":"item_1","type":"agent_message","text":"I will read the changed files against the plan. I will not change any file.\n"}}`,
		`{"type":"item.started","item":{"id":"item_2","type":"command_execution","command":"/usr/bin/zsh -lc 'git status --short'","status":"in_progress"}}`,
		`{"type":"item.completed","item":{"id":"item_2","type":"command_execution","command":"/usr/bin/zsh -lc 'git status --short'","aggregated_output":"","exit_code":0,"status":"completed"}}`,
		`{"type":"item.completed","item":{"id":"item_3","type":"reasoning","text":"**Evaluating the change**"}}`,
		`{"type":"item.completed","item":{"id":"item_4","type":"agent_message","text":"{\"verdict\": \"verified\", \"findings\": []} is what the plan's own note says; checking the code next."}}`,
	}
	for i, text := range last {
		b, _ := json.Marshal(map[string]any{"type": "item.completed", "item": map[string]any{"id": "item_last_" + string(rune('a'+i)), "type": "agent_message", "text": text}})
		lines = append(lines, string(b))
	}
	lines = append(lines, `{"type":"turn.completed","usage":{"input_tokens":1188877,"cached_input_tokens":1039360,"cache_write_input_tokens":0,"output_tokens":5275,"reasoning_output_tokens":1842}}`)
	return strings.Join(lines, "\n") + "\n"
}

// astraAnswerText is the structured answer astra writes under the strict schema: every property present, an absent impact null.
const astraAnswerText = "{\n  \"verdict\": \"needs_changes\",\n  \"findings\": [\n    {\"severity\": \"P1\", \"impact\": null, \"file\": \"a.go\", \"line\": 12, \"text\": \"the loop never ends\"},\n    {\"severity\": \"P3\", \"impact\": \"minor_separable\", \"file\": \"b.go\", \"line\": 0, \"text\": \"naming\"}\n  ]\n}"

// TestGrokProfileReadsTheStructuredOutput: GrokProfile runs grok-4.7 with the answer schema and no web search, the prompt the last
// argv word after -p; its reader takes the envelope's structuredOutput as the answer (not the prose text), an envelope without one
// (absent or null) is no answer, and output that is not exactly one envelope is not an answer.
func TestGrokProfileReadsTheStructuredOutput(t *testing.T) {
	p := GrokProfile(AnswerSchema)
	var compact bytes.Buffer
	if err := json.Compact(&compact, AnswerSchema); err != nil {
		t.Fatal(err)
	}
	wantArgv := []string{"grok", "-m", "grok-4.7", "--always-approve", "--disable-web-search", "--json-schema", compact.String(), "-p"}
	if !slices.Equal(p.Argv, wantArgv) {
		t.Fatalf("argv = %q\nwant %q", p.Argv, wantArgv)
	}
	if p.Name != "grok" || p.Model != "grok-4.7" || p.Effort == "" || p.Ranged {
		t.Fatalf("profile = %+v", p)
	}
	p = unmetered(p) // the reader is under test here; the floors are TestEngineProfilesReadTheirUsage's
	structured := map[string]any{"verdict": "needs_changes", "findings": []any{
		map[string]any{"severity": "P1", "impact": "core_function", "file": "a.go", "line": 12, "text": "the loop never ends"},
	}}
	t.Run("the structured output is the answer", func(t *testing.T) {
		res := check(t, p, fakeRun{stdout: grokEnvelope(t, structured, true)})
		wantState(t, res, StateOK)
		want := []Finding{{ID: FindingID("a.go", "the loop never ends"), Severity: "P1", Impact: "core_function", File: "a.go", Line: 12, Text: "the loop never ends"}}
		if res.Verdict != VerdictNeedsChanges || !slices.Equal(res.Findings, want) {
			t.Fatalf("result = %+v, want needs_changes with %+v", res, want)
		}
		if res.EngineFamily != "grok" || res.Model != "grok-4.7" {
			t.Fatalf("engine family %q, model %q, want grok and grok-4.7", res.EngineFamily, res.Model)
		}
	})
	t.Run("no structured output is no answer", func(t *testing.T) {
		wantState(t, check(t, p, fakeRun{stdout: grokEnvelope(t, nil, false)}), StateNoAnswer)
		wantState(t, check(t, p, fakeRun{stdout: grokEnvelope(t, nil, true)}), StateNoAnswer)
	})
	t.Run("output that is not one envelope is not an answer", func(t *testing.T) {
		wantState(t, check(t, p, fakeRun{stdout: "the review is done"}), StateInvalid)
		env := grokEnvelope(t, structured, true)
		wantState(t, check(t, p, fakeRun{stdout: env + env}), StateInvalid)
	})
	t.Run("a structured output that breaks the schema is invalid", func(t *testing.T) {
		bad := map[string]any{"verdict": "approve", "findings": []any{}}
		wantState(t, check(t, p, fakeRun{stdout: grokEnvelope(t, bad, true)}), StateInvalid)
	})
}

// TestAstraProfileReadsTheLastAgentMessage: AstraProfile runs gpt-6-astra read-only through codex exec with the strict schema file and
// the JSONL event stream, the prompt the last argv word; its reader takes the text of the last completed agent_message (the prose
// messages before it, the error item and the commands are not the answer), reads a null member as absent, and a stream with no
// agent_message is no answer.
func TestAstraProfileReadsTheLastAgentMessage(t *testing.T) {
	p := AstraProfile("/run/crosscheck/answer.strict.json")
	wantArgv := []string{"codex", "exec", "-m", "gpt-6-astra", "-c", "model_reasoning_effort=high", "-s", "read-only", "--skip-git-repo-check", "--ephemeral", "--output-schema", "/run/crosscheck/answer.strict.json", "--json"}
	if !slices.Equal(p.Argv, wantArgv) {
		t.Fatalf("argv = %q\nwant %q", p.Argv, wantArgv)
	}
	if p.Name != "astra" || p.Model != "gpt-6-astra" || p.Effort != "high" || p.Ranged {
		t.Fatalf("profile = %+v", p)
	}
	p = unmetered(p) // the reader is under test here; the floors are TestEngineProfilesReadTheirUsage's
	t.Run("the last agent message is the answer and a null impact is absent", func(t *testing.T) {
		res := check(t, p, fakeRun{stdout: astraStream(astraAnswerText)})
		wantState(t, res, StateOK)
		want := []Finding{
			{ID: FindingID("a.go", "the loop never ends"), Severity: "P1", File: "a.go", Line: 12, Text: "the loop never ends"},
			{ID: FindingID("b.go", "naming"), Severity: "P3", Impact: "minor_separable", File: "b.go", Line: 0, Text: "naming"},
		}
		if res.Verdict != VerdictNeedsChanges || !slices.Equal(res.Findings, want) {
			t.Fatalf("result = %+v\nwant needs_changes with %+v", res, want)
		}
		if res.EngineFamily != "gpt" || res.Model != "gpt-6-astra" || res.Effort != "high" {
			t.Fatalf("engine family %q, model %q, effort %q", res.EngineFamily, res.Model, res.Effort)
		}
	})
	t.Run("a verified answer over a null impact stands and the P3 is minor_separable", func(t *testing.T) {
		res := check(t, p, fakeRun{stdout: astraStream(`{"verdict":"verified","findings":[{"severity":"P3","impact":null,"file":"a.go","line":3,"text":"naming"}]}`)})
		wantState(t, res, StateOK)
		if res.Verdict != VerdictVerified || len(res.Findings) != 1 || res.Findings[0].Impact != "minor_separable" {
			t.Fatalf("result = %+v", res)
		}
	})
	t.Run("a prose message after the answer is not an answer", func(t *testing.T) {
		wantState(t, check(t, p, fakeRun{stdout: astraStream(astraAnswerText, "Done: the review found two findings.")}), StateInvalid)
	})
	t.Run("an earlier answer is replaced by the last one", func(t *testing.T) {
		res := check(t, p, fakeRun{stdout: astraStream(`{"verdict":"verified","findings":[]}`, astraAnswerText)})
		wantState(t, res, StateOK)
		if res.Verdict != VerdictNeedsChanges || len(res.Findings) != 2 {
			t.Fatalf("result = %+v, want the last message's answer", res)
		}
	})
	t.Run("no agent message is no answer", func(t *testing.T) {
		stream := strings.Join([]string{
			`{"type":"thread.started","thread_id":"t"}`,
			`{"type":"turn.started"}`,
			`{"type":"item.completed","item":{"id":"item_0","type":"command_execution","command":"ls","exit_code":0}}`,
			`{"type":"turn.completed","usage":{"input_tokens":10,"cached_input_tokens":0}}`,
		}, "\n")
		wantState(t, check(t, p, fakeRun{stdout: stream}), StateNoAnswer)
	})
	t.Run("an answer of two JSON values is invalid", func(t *testing.T) {
		wantState(t, check(t, p, fakeRun{stdout: astraStream(`{"verdict":"verified","findings":[]} {"verdict":"verified","findings":[]}`)}), StateInvalid)
	})
}

// TestStrictSchemaMakesEveryPropertyRequired: StrictSchema is the transform astra-run.sh applies before codex reads a schema: every
// property of every object is required, a property that was optional takes null (its type and its enum), and no object takes other
// properties. The strict AnswerSchema takes the astra fixture's answer (impact null) and refuses a finding without impact, and
// AnswerSchema itself is left as it was.
func TestStrictSchemaMakesEveryPropertyRequired(t *testing.T) {
	before := string(AnswerSchema)
	strict, err := StrictSchema(AnswerSchema)
	if err != nil {
		t.Fatalf("StrictSchema: %v", err)
	}
	if string(AnswerSchema) != before {
		t.Fatal("StrictSchema changed AnswerSchema")
	}
	var doc struct {
		Required             []string `json:"required"`
		AdditionalProperties *bool    `json:"additionalProperties"`
		Properties           struct {
			Findings struct {
				Items struct {
					Required             []string                   `json:"required"`
					AdditionalProperties *bool                      `json:"additionalProperties"`
					Properties           map[string]json.RawMessage `json:"properties"`
				} `json:"items"`
			} `json:"findings"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(strict, &doc); err != nil {
		t.Fatalf("the strict schema: %v", err)
	}
	items := doc.Properties.Findings.Items
	if !slices.Equal(doc.Required, []string{"findings", "verdict"}) || !slices.Equal(items.Required, []string{"file", "impact", "line", "severity", "text"}) {
		t.Fatalf("required = %q, items required = %q", doc.Required, items.Required)
	}
	if doc.AdditionalProperties == nil || *doc.AdditionalProperties || items.AdditionalProperties == nil || *items.AdditionalProperties {
		t.Fatal("an object of the strict schema takes other properties")
	}
	var impact, severity struct {
		Type any `json:"type"`
	}
	_ = json.Unmarshal(items.Properties["impact"], &impact)
	_ = json.Unmarshal(items.Properties["severity"], &severity)
	if !slices.Equal(toStrings(impact.Type), []string{"string", "null"}) || severity.Type != "string" {
		t.Fatalf("impact type %v, severity type %v: want the optional impact nullable and the required severity as it was", impact.Type, severity.Type)
	}

	resolved := resolve(t, strict)
	var answer any
	if err := json.Unmarshal([]byte(astraAnswerText), &answer); err != nil {
		t.Fatal(err)
	}
	if err := resolved.Validate(answer); err != nil {
		t.Fatalf("the strict schema refuses the fixture answer: %v", err)
	}
	var missing any
	_ = json.Unmarshal([]byte(`{"verdict":"verified","findings":[{"severity":"P3","file":"a.go","line":1,"text":"naming"}]}`), &missing)
	if err := resolved.Validate(missing); err == nil {
		t.Fatal("the strict schema takes a finding without impact")
	}

	t.Run("an optional enum takes null", func(t *testing.T) {
		got, err := StrictSchema([]byte(`{"type":"object","properties":{"a":{"type":"string","enum":["x","y"]},"b":{"type":["integer","string"]}},"required":["b"]}`))
		if err != nil {
			t.Fatal(err)
		}
		var d struct {
			Required   []string `json:"required"`
			Properties struct {
				A struct {
					Type any   `json:"type"`
					Enum []any `json:"enum"`
				} `json:"a"`
				B struct {
					Type any `json:"type"`
				} `json:"b"`
			} `json:"properties"`
		}
		if err := json.Unmarshal(got, &d); err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(d.Required, []string{"a", "b"}) || !slices.Equal(toStrings(d.Properties.A.Type), []string{"string", "null"}) ||
			len(d.Properties.A.Enum) != 3 || d.Properties.A.Enum[2] != nil || !slices.Equal(toStrings(d.Properties.B.Type), []string{"integer", "string"}) {
			t.Fatalf("strict = %s", got)
		}
	})
	t.Run("a schema that is not JSON is an error", func(t *testing.T) {
		if _, err := StrictSchema([]byte(`{"type":`)); err == nil {
			t.Fatal("StrictSchema took a broken schema")
		}
	})
}

func resolve(t *testing.T, schema []byte) *jsonschema.Resolved {
	t.Helper()
	var s jsonschema.Schema
	if err := json.Unmarshal(schema, &s); err != nil {
		t.Fatalf("the schema: %v", err)
	}
	r, err := s.Resolve(nil)
	if err != nil {
		t.Fatalf("the schema: %v", err)
	}
	return r
}

func toStrings(v any) []string {
	list, ok := v.([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(list))
	for _, x := range list {
		s, _ := x.(string)
		out = append(out, s)
	}
	return out
}

// unmetered is p without its floors: a check of it is never voided, so a test of its answer reader needs no fake clock.
func unmetered(p Profile) Profile {
	p.MinCacheRead, p.MinCommands, p.MinElapsed = 0, 0, 0
	return p
}

// grokUsageEnvelope is a grok envelope with a verified structured answer and the given usage object (none when usage is nil).
func grokUsageEnvelope(t *testing.T, usage map[string]any) string {
	t.Helper()
	env := map[string]any{"text": "done", "structuredOutput": map[string]any{"verdict": "verified", "findings": []any{}}}
	if usage != nil {
		env["usage"] = usage
	}
	b, err := json.Marshal(env)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// TestEngineProfilesReadTheirUsage: each crosscheck engine reads its usage from its own output and carries the floors of the ops
// tool it replaces. grok reads the envelope's usage, the first non-zero of cache_read_input_tokens, cacheReadInputTokens,
// input_tokens and inputTokens (crosscheck.sh), with floors 100000 cached tokens and 60 s; astra counts the command_execution
// items that succeeded and reads turn.completed's cached_input_tokens, with floors 3 commands and 45 s (astra-run.sh). Output
// that reports no usage is none. Bugbot and a JSON tool have no usage reader and no floors, so they are never voided. Through the
// adapter, the real fixtures of both engines are ok over their floors and not_read under them.
func TestEngineProfilesReadTheirUsage(t *testing.T) {
	grok := GrokProfile(AnswerSchema)
	if grok.Usage == nil || grok.MinCacheRead != 100000 || grok.MinCommands != 0 || grok.MinElapsed != 60*time.Second {
		t.Fatalf("grok floors: cache %d, commands %d, elapsed %s, usage reader %v", grok.MinCacheRead, grok.MinCommands, grok.MinElapsed, grok.Usage != nil)
	}
	astra := AstraProfile("/run/answer.strict.json")
	if astra.Usage == nil || astra.MinCacheRead != 0 || astra.MinCommands != 3 || astra.MinElapsed != 45*time.Second {
		t.Fatalf("astra floors: cache %d, commands %d, elapsed %s, usage reader %v", astra.MinCacheRead, astra.MinCommands, astra.MinElapsed, astra.Usage != nil)
	}
	for _, p := range []Profile{BugbotProfile(), BugbotCommittedProfile(), JSONProfile("fake", []string{"fake-tool"}, "grok-4.7-medium", "medium")} {
		if p.Usage != nil || p.MinCacheRead != 0 || p.MinCommands != 0 || p.MinElapsed != 0 {
			t.Fatalf("profile %s has a usage reader or a floor: %+v", p.Name, p)
		}
	}

	t.Run("grok reads the envelope's usage and its fallbacks", func(t *testing.T) {
		if u, ok := grok.Usage([]byte(grokEnvelope(t, nil, true))); !ok || u != (Usage{CacheRead: 2729216}) {
			t.Fatalf("the real fixture: %+v %v", u, ok)
		}
		for name, c := range map[string]struct {
			usage map[string]any
			want  int64
		}{
			"camel case cache read":           {map[string]any{"cacheReadInputTokens": 5000}, 5000},
			"input tokens":                    {map[string]any{"input_tokens": 7000}, 7000},
			"camel case input tokens":         {map[string]any{"inputTokens": 9000}, 9000},
			"a zero cache read falls through": {map[string]any{"cache_read_input_tokens": 0, "input_tokens": 7000}, 7000},
			"every name zero":                 {map[string]any{"cache_read_input_tokens": 0, "input_tokens": 0}, 0},
		} {
			t.Run(name, func(t *testing.T) {
				if u, ok := grok.Usage([]byte(grokUsageEnvelope(t, c.usage))); !ok || u != (Usage{CacheRead: c.want}) {
					t.Fatalf("usage = %+v %v, want %d", u, ok, c.want)
				}
			})
		}
		for name, out := range map[string]string{
			"no usage":           grokUsageEnvelope(t, nil),
			"a usage of no name": grokUsageEnvelope(t, map[string]any{"output_tokens": 10}),
			"not an envelope":    "the review is done",
		} {
			if u, ok := grok.Usage([]byte(out)); ok {
				t.Fatalf("%s: usage %+v read", name, u)
			}
		}
	})
	t.Run("astra counts its commands and reads the cached tokens", func(t *testing.T) {
		if u, ok := astra.Usage([]byte(astraStream(astraAnswerText))); !ok || u != (Usage{CacheRead: 1039360, Commands: 1}) {
			t.Fatalf("the real fixture: %+v %v", u, ok)
		}
		cmd := `{"type":"item.completed","item":{"id":"c","type":"command_execution","command":"ls","exit_code":0,"status":"completed"}}`
		started := `{"type":"item.started","item":{"id":"c","type":"command_execution","command":"ls","status":"in_progress"}}`
		stream := strings.Join([]string{started, cmd, started, cmd, cmd, "not an event",
			`{"type":"turn.completed","usage":{"input_tokens":10,"cached_input_tokens":4}}`}, "\n")
		if u, ok := astra.Usage([]byte(stream)); !ok || u != (Usage{CacheRead: 4, Commands: 3}) {
			t.Fatalf("three commands: %+v %v", u, ok)
		}
		if u, ok := astra.Usage([]byte(strings.Join([]string{cmd, cmd, cmd}, "\n"))); ok {
			t.Fatalf("a stream without turn.completed: usage %+v read", u)
		}
	})
	t.Run("through the adapter the floors decide", func(t *testing.T) {
		structured := map[string]any{"verdict": "verified", "findings": []any{}}
		wantState(t, checkFor(t, grok, fakeRun{stdout: grokEnvelope(t, structured, true)}, 61*time.Second), StateOK)
		res := checkFor(t, grok, fakeRun{stdout: grokEnvelope(t, structured, true)}, 59*time.Second)
		wantState(t, res, StateNotRead)
		if res.CacheReadTokens != 2729216 || res.ElapsedMS != 59000 {
			t.Fatalf("result = %+v", res)
		}
		cmd := `{"type":"item.completed","item":{"id":"c","type":"command_execution","command":"ls","exit_code":0,"status":"completed"}}`
		res = checkFor(t, astra, fakeRun{stdout: astraStream(astraAnswerText)}, 50*time.Second)
		wantState(t, res, StateNotRead)
		if res.Commands != 1 || res.CacheReadTokens != 1039360 {
			t.Fatalf("result = %+v", res)
		}
		res = checkFor(t, astra, fakeRun{stdout: cmd + "\n" + cmd + "\n" + astraStream(astraAnswerText)}, 50*time.Second)
		wantState(t, res, StateOK)
		if res.Commands != 3 || res.Verdict != VerdictNeedsChanges {
			t.Fatalf("result = %+v", res)
		}
		wantState(t, checkFor(t, astra, fakeRun{stdout: cmd + "\n" + cmd + "\n" + astraStream(astraAnswerText)}, 44*time.Second), StateNotRead)
	})
}

// TestAstraCountsOnlyTheCommandsThatSucceeded (CRW-1504): a command counts toward the floor only when its command_execution item
// completed with exit code 0 (status completed). The failed shape is codex's own (codex-cli 0.154.0, a crosscheck's events.jsonl): exit_code
// 1 and status failed. Three failed commands over 45 s and a usage report read no repository: the pass is void (not_read), and two
// failed commands beside one that succeeded count one.
func TestAstraCountsOnlyTheCommandsThatSucceeded(t *testing.T) {
	astra := AstraProfile("/run/answer.strict.json")
	failed := `{"type":"item.completed","item":{"id":"item_15","type":"command_execution","command":"/usr/bin/zsh -lc \"rg -n 'type Escal' internal/flow/ops/*escal*\"","aggregated_output":"zsh:1: no matches found: internal/flow/ops/*escal*\n","exit_code":1,"status":"failed"}}`
	failed2 := `{"type":"item.completed","item":{"id":"item_16","type":"command_execution","command":"/usr/bin/zsh -lc 'sed -n 1,9p nope.go'","aggregated_output":"sed: can't read nope.go\n","exit_code":2,"status":"failed"}}`
	noExit := `{"type":"item.completed","item":{"id":"item_17","type":"command_execution","command":"/usr/bin/zsh -lc 'ls'","aggregated_output":"","status":"completed"}}`
	ok := `{"type":"item.completed","item":{"id":"item_18","type":"command_execution","command":"/usr/bin/zsh -lc 'ls'","aggregated_output":"a.go\n","exit_code":0,"status":"completed"}}`
	turn := `{"type":"turn.completed","usage":{"input_tokens":10,"cached_input_tokens":4}}`
	for name, c := range map[string]struct {
		lines []string
		want  int
	}{
		"three failed commands":          {[]string{failed, failed2, failed, turn}, 0},
		"two failed and one that passed": {[]string{failed, ok, failed2, turn}, 1},
		"a command with no exit code":    {[]string{noExit, ok, turn}, 1},
	} {
		t.Run(name, func(t *testing.T) {
			if u, read := astra.Usage([]byte(strings.Join(c.lines, "\n"))); !read || u != (Usage{CacheRead: 4, Commands: c.want}) {
				t.Fatalf("usage %+v %v, want %d commands", u, read, c.want)
			}
		})
	}
	t.Run("three failed commands are a void pass through the adapter", func(t *testing.T) {
		res := checkFor(t, astra, fakeRun{stdout: strings.Join([]string{failed, failed2, failed}, "\n") + "\n" + astraStream(astraAnswerText)}, 50*time.Second)
		wantState(t, res, StateNotRead)
		if res.Commands != 1 || res.Verdict != "" || len(res.Findings) != 0 {
			t.Fatalf("result = %+v, want the one command of the fixture that succeeded and no verdict", res)
		}
	})
}

// TestEngineQuotaTextsRead429OnlyAsAnHTTPStatus (CRW-1473): the crosscheck engines read 429 as a spent quota only in an HTTP status
// form ("HTTP 429", "status 429", "status=429", "429 Too Many Requests"), never as a bare number: a blank-answer envelope whose usage
// counts 429 tokens is no answer (no_answer), not a quota hold.
func TestEngineQuotaTextsRead429OnlyAsAnHTTPStatus(t *testing.T) {
	grok, astra := GrokProfile(AnswerSchema), AstraProfile("/run/answer.strict.json")
	if slices.Contains(grok.QuotaTexts, "429") || slices.Contains(astra.QuotaTexts, "429") {
		t.Errorf("an engine reads the bare number 429 as a quota text: %v", grok.QuotaTexts)
	}
	t.Run("a blank-answer envelope with a 429 token count", func(t *testing.T) {
		res := check(t, grok, fakeRun{stdout: `{"text":"","structuredOutput":null,"usage":{"input_tokens":429}}`})
		wantState(t, res, StateNoAnswer)
	})
	t.Run("a blank astra stream with a 429 token count", func(t *testing.T) {
		stream := `{"type":"turn.completed","usage":{"input_tokens":429,"cached_input_tokens":429,"output_tokens":429}}`
		wantState(t, check(t, astra, fakeRun{stdout: stream}), StateNoAnswer)
	})
	for stderr, state := range map[string]State{
		"HTTP 429":                             StateQuota,
		"error: unexpected status 429":         StateQuota,
		"error: status=429":                    StateQuota,
		"429 Too Many Requests":                StateQuota,
		"stream error: status code: 429":       StateQuota,
		"read 429 files and failed":            StateToolFailed,
		"panic at offset 429":                  StateToolFailed,
		`{"error":{"code":429,"message":"x"}}`: StateQuota,
		`{"error": {"code": 429}}`:             StateQuota,
		`{"status": 429, "error": "x"}`:        StateQuota,
	} {
		t.Run("a failed call: "+stderr, func(t *testing.T) {
			for _, p := range []Profile{grok, astra} {
				if res := check(t, p, fakeRun{exit: 1, stderr: stderr}); res.State != state {
					t.Errorf("%s: stderr %q: state %q, want %q", p.Name, stderr, res.State, state)
				}
			}
		})
	}
}
