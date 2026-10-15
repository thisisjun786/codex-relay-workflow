package reviewadapter

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"slices"
	"time"
)

// The review engines of the crosscheck (CRW-1473): astra (gpt-6-astra through codex exec, family gpt) and grok (grok-4.7, family
// grok), run as the ops crosscheck tool runs them (/state/crw/mgmt/tools-v3/astra-run.sh and crosscheck.sh). ExecRun appends the
// prompt as the last argv word and runs the tool in the request's worktree; the schema the tool answers under is part of the argv,
// since ExecRun never passes Request.Schema to the tool.
const (
	grokModel   = "grok-4.7"
	astraModel  = "gpt-6-astra"
	astraEffort = "high"
	// grokEffort is the effort word of a grok call: the call names no effort, so the tool's own default applies.
	grokEffort = "default"
)

// engineQuotaTexts are the texts that mean an engine's quota is spent. They are UNCONFIRMED: no real quota sample of either tool has
// been captured. "upgrade" is not among them, since codex logs "426 Upgrade Required" on stderr of an ordinary run. They are matched
// only on a failed call or a blank answer, so a finding that quotes one is unaffected.
var engineQuotaTexts = []string{"usage limit", "rate limit", "resource_exhausted", "too many requests", "429", "quota exceeded"}

// GrokProfile is grok-4.7 through the grok CLI with schema as its --json-schema: `grok -m grok-4.7 --always-approve
// --disable-web-search --json-schema <schema> -p <prompt>`, the prompt being the word ExecRun appends after -p. grok prints one JSON
// envelope; its answer is the envelope's structuredOutput (grokAnswer) and its usage the envelope's usage (grokUsage). Its floors are
// the ops tool's (crosscheck.sh): a pass that read fewer than 100000 cached tokens or ran under 60 s read no repository (not_read).
func GrokProfile(schema []byte) Profile {
	word := string(schema)
	var compact bytes.Buffer
	if json.Compact(&compact, schema) == nil {
		word = compact.String()
	}
	return Profile{
		Name:       "grok",
		Argv:       []string{"grok", "-m", grokModel, "--always-approve", "--disable-web-search", "--json-schema", word, "-p"},
		Model:      grokModel,
		Effort:     grokEffort,
		QuotaTexts: slices.Clone(engineQuotaTexts),
		Answer:     grokAnswer,
		Usage:      grokUsage,
		// the floors of crosscheck.sh (MIN_CACHE, MIN_SECS)
		MinCacheRead: 100000,
		MinElapsed:   60 * time.Second,
	}
}

// grokUsage reads the usage object of grok's envelope as the ops tool reads it (crosscheck.sh): the cached tokens are the first
// non-zero of cache_read_input_tokens, cacheReadInputTokens, input_tokens and inputTokens (the input tokens stand in when the cache
// read is absent), zero when each it has is zero. grok reports no commands. Output that is not exactly one envelope, an envelope
// without a usage object, or a usage object with none of the four names, reports no usage.
func grokUsage(stdout []byte) (Usage, bool) {
	var env struct {
		Usage map[string]json.RawMessage `json:"usage"`
	}
	dec := json.NewDecoder(bytes.NewReader(stdout))
	if dec.Decode(&env) != nil || onlyValue(dec) != nil || env.Usage == nil {
		return Usage{}, false
	}
	found := false
	for _, name := range []string{"cache_read_input_tokens", "cacheReadInputTokens", "input_tokens", "inputTokens"} {
		raw, ok := env.Usage[name]
		if !ok {
			continue
		}
		var n int64
		if json.Unmarshal(raw, &n) != nil {
			continue
		}
		found = true
		if n != 0 {
			return Usage{CacheRead: n}, true
		}
	}
	return Usage{}, found
}

// grokAnswer reads grok's envelope: exactly one JSON document, whose structuredOutput, re-marshalled, is the answer. An envelope
// without structuredOutput, or with a null one, is a blank answer (no_answer); the envelope's prose text is never the answer.
func grokAnswer(stdout []byte, _ Request) ([]byte, error) {
	var env struct {
		StructuredOutput json.RawMessage `json:"structuredOutput"`
	}
	dec := json.NewDecoder(bytes.NewReader(stdout))
	if err := dec.Decode(&env); err != nil {
		return nil, fmt.Errorf("the output is not grok's JSON envelope: %v", err)
	}
	if err := onlyValue(dec); err != nil {
		return nil, fmt.Errorf("the output is more than one JSON envelope: %v", err)
	}
	so := bytes.TrimSpace(env.StructuredOutput)
	if len(so) == 0 || string(so) == "null" {
		return nil, nil
	}
	var out bytes.Buffer
	if err := json.Compact(&out, so); err != nil {
		return nil, fmt.Errorf("the envelope's structuredOutput: %v", err)
	}
	return out.Bytes(), nil
}

// AstraProfile is gpt-6-astra through codex exec, read-only, answering under the strict schema in schemaFile (StrictSchema of the
// request's schema, written by the caller): `codex exec -m gpt-6-astra -c model_reasoning_effort=high -s read-only
// --skip-git-repo-check --ephemeral --output-schema <schemaFile> --json <prompt>`, the prompt being the positional word ExecRun
// appends. codex prints a JSONL event stream; its answer is the text of the last completed agent_message (astraAnswer) and its usage
// the commands it ran that succeeded and the cached tokens of its turn (astraUsage). Its floors are the ops tool's (astra-run.sh): a
// pass that ran fewer than 3 commands that succeeded or under 45 s read no repository (not_read).
func AstraProfile(schemaFile string) Profile {
	return Profile{
		Name: "astra",
		Argv: []string{"codex", "exec", "-m", astraModel, "-c", "model_reasoning_effort=" + astraEffort, "-s", "read-only",
			"--skip-git-repo-check", "--ephemeral", "--output-schema", schemaFile, "--json"},
		Model:      astraModel,
		Effort:     astraEffort,
		QuotaTexts: slices.Clone(engineQuotaTexts),
		Answer:     astraAnswer,
		Usage:      astraUsage,
		// the floors of astra-run.sh (MIN_CMDS, MIN_SECS)
		MinCommands: 3,
		MinElapsed:  45 * time.Second,
	}
}

// astraUsage reads codex's JSONL event stream: the commands are the item.completed events whose item is a command_execution that
// succeeded (status completed and exit_code 0; a failed one is status failed with its exit code, and one with no exit code is not
// counted), so commands that read nothing do not pass the floor, and the cached tokens are the cached_input_tokens of the last
// turn.completed event's usage. A line that is not a JSON event is skipped; a stream with no turn.completed event that has a usage
// object reports no usage.
func astraUsage(stdout []byte) (Usage, bool) {
	var u Usage
	ok := false
	sc := bufio.NewScanner(bytes.NewReader(stdout))
	sc.Buffer(make([]byte, 0, 64<<10), maxOutput)
	for sc.Scan() {
		var ev struct {
			Type string `json:"type"`
			Item struct {
				Type     string `json:"type"`
				Status   string `json:"status"`
				ExitCode *int   `json:"exit_code"`
			} `json:"item"`
			Usage *struct {
				CachedInputTokens int64 `json:"cached_input_tokens"`
			} `json:"usage"`
		}
		if json.Unmarshal(bytes.TrimSpace(sc.Bytes()), &ev) != nil {
			continue
		}
		switch {
		case ev.Type == "item.completed" && ev.Item.Type == "command_execution":
			if ev.Item.Status == "completed" && ev.Item.ExitCode != nil && *ev.Item.ExitCode == 0 {
				u.Commands++
			}
		case ev.Type == "turn.completed" && ev.Usage != nil:
			u.CacheRead, ok = ev.Usage.CachedInputTokens, true
		}
	}
	if sc.Err() != nil {
		return Usage{}, false
	}
	return u, ok
}

// astraAnswer reads codex's JSONL event stream: the answer is the text of the last item.completed event whose item is an
// agent_message (the earlier ones are prose progress notes), with every null member of its objects dropped, since the strict schema
// makes an absent optional member null. A line that is not a JSON event is skipped, as the ops tool skips it; a stream with no
// completed agent_message is a blank answer (no_answer). A text that is not exactly one JSON value is given as it is, for the
// adapter to refuse (invalid).
func astraAnswer(stdout []byte, _ Request) ([]byte, error) {
	var last *string
	sc := bufio.NewScanner(bytes.NewReader(stdout))
	sc.Buffer(make([]byte, 0, 64<<10), maxOutput)
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		var ev struct {
			Type string `json:"type"`
			Item struct {
				Type string  `json:"type"`
				Text *string `json:"text"`
			} `json:"item"`
		}
		if json.Unmarshal(line, &ev) != nil {
			continue
		}
		if ev.Type == "item.completed" && ev.Item.Type == "agent_message" {
			text := ""
			if ev.Item.Text != nil {
				text = *ev.Item.Text
			}
			last = &text
		}
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("the output is not codex's event stream: %v", err)
	}
	if last == nil || len(bytes.TrimSpace([]byte(*last))) == 0 {
		return nil, nil
	}
	text := []byte(*last)
	dec := json.NewDecoder(bytes.NewReader(text))
	dec.UseNumber()
	var v any
	if dec.Decode(&v) != nil || onlyValue(dec) != nil {
		return text, nil
	}
	return json.Marshal(dropNulls(v))
}

// dropNulls removes every null member of the objects in v, at any depth; a null array element is kept.
func dropNulls(v any) any {
	switch x := v.(type) {
	case map[string]any:
		for k, m := range x {
			if m == nil {
				delete(x, k)
				continue
			}
			x[k] = dropNulls(m)
		}
	case []any:
		for i := range x {
			x[i] = dropNulls(x[i])
		}
	}
	return v
}

// StrictSchema is the transform the ops astra tool applies to a schema before codex reads it (astra-run.sh): in every object of
// the schema (a node whose type is object or that has properties), each property that was not required takes null (null is added to
// its type and, when it has one, to its enum), every property becomes required (in name order), and no other property is allowed.
// schema itself is not changed.
func StrictSchema(schema []byte) ([]byte, error) {
	dec := json.NewDecoder(bytes.NewReader(schema))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, fmt.Errorf("reviewadapter: the schema is not JSON: %w", err)
	}
	if err := onlyValue(dec); err != nil {
		return nil, fmt.Errorf("reviewadapter: the schema is more than one JSON value: %w", err)
	}
	return json.Marshal(strictNode(v))
}

func strictNode(v any) any {
	switch n := v.(type) {
	case []any:
		for i := range n {
			n[i] = strictNode(n[i])
		}
		return n
	case map[string]any:
		for k, m := range n {
			n[k] = strictNode(m)
		}
		props, hasProps := n["properties"].(map[string]any)
		if n["type"] != "object" && !hasProps {
			return n
		}
		required := map[string]bool{}
		if list, ok := n["required"].([]any); ok {
			for _, r := range list {
				if s, ok := r.(string); ok {
					required[s] = true
				}
			}
		}
		names := make([]string, 0, len(props))
		for k, p := range props {
			names = append(names, k)
			if prop, ok := p.(map[string]any); ok && !required[k] {
				nullable(prop)
			}
		}
		slices.Sort(names)
		all := make([]any, len(names))
		for i, k := range names {
			all[i] = k
		}
		n["required"] = all
		n["additionalProperties"] = false
		return n
	}
	return v
}

// nullable lets an optional property be null: null is added to its type (a word or a list) and to its enum when it has one.
func nullable(prop map[string]any) {
	switch t := prop["type"].(type) {
	case string:
		prop["type"] = []any{t, "null"}
	case []any:
		if !slices.ContainsFunc(t, func(x any) bool { s, ok := x.(string); return ok && s == "null" }) {
			prop["type"] = append(t, "null")
		}
	}
	if enum, ok := prop["enum"].([]any); ok && !slices.ContainsFunc(enum, func(x any) bool { return x == nil }) {
		prop["enum"] = append(enum, nil)
	}
}
