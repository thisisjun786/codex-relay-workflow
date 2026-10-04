// Package ledger is the interview question and answer ledger: the Go form of CXC v0.2.40 pabcd-state/src/interview-ledger.ts
// (commit 3c1459ac). It records what a request_user_input round asked and answered, one question_asked and one answer_recorded row
// per question, in .crw/interviews/<session>.jsonl, and tells which interview dimensions rest on an answered question. The file
// also holds the scan rows of internal/pabcd/state; each reader skips the other's rows.
//
// Behaviour is ported as-is, oracle defects included (docs/port-cxc/known-defects.md, "Found by the interview answer ledger port"),
// with two differences:
//
//   - Appending to a file whose last line has no line feed first writes one: the oracle joins the new row to that line and both are
//     lost (a record-file data-loss defect, fixed by decision). The guard covers the tail seen before the write; a sibling writer
//     (state.AppendInterviewEvent, which has no guard or lock) that appends in between or leaves a partial line later is not covered.
//   - A payload is the value encoding/json decodes, numbers kept as json.Number; any other Go value is no payload. A lone surrogate
//     escape reads as U+FFFD (a Go string cannot hold one), an invalid UTF-8 byte is written as U+FFFD, and a document nested deeper
//     than 10,000 levels reads as malformed.
//
// Nothing here is a lock: the dedup check and the append are separate steps, as in the oracle.
package ledger

import (
	"encoding/json"
	"io"
	"os"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/text"
)

// ParsedQuestion is one question of a request_user_input tool_input.
type ParsedQuestion struct {
	QuestionID string
	Question   string
}

// parseJSON is JSON.parse: one value and nothing but JSON whitespace after it. A number no float64 holds stays a json.Number.
func parseJSON(s string) (any, bool) {
	dec := json.NewDecoder(strings.NewReader(s))
	dec.UseNumber()
	var v any
	if dec.Decode(&v) != nil {
		return nil, false
	}
	_, err := dec.Token()
	return v, err == io.EOF
}

// asRecord is the payload as a record: the record itself, what a JSON string decodes to (a string inside it is decoded again, an
// array gives its first record), or the first record of an array. Anything else, and any text that is no JSON, gives none.
func asRecord(v any) (map[string]any, bool) {
	switch v := v.(type) {
	case map[string]any:
		return v, true
	case string:
		parsed, ok := parseJSON(v)
		if !ok {
			return nil, false
		}
		switch p := parsed.(type) {
		case map[string]any:
			return p, true
		case string:
			return asRecord(p)
		}
		return firstRecord(parsed)
	case []any:
		return firstRecord(v)
	}
	return nil, false
}

// firstRecord is the first record among the first eight parts of a content-block array: a record with answers or questions, else
// what its text, output or content string decodes to, else the record itself; a string part counts when it decodes to a record.
func firstRecord(v any) (map[string]any, bool) {
	parts, ok := v.([]any)
	if !ok {
		return nil, false
	}
	for _, part := range parts[:min(len(parts), 8)] {
		switch p := part.(type) {
		case map[string]any:
			if _, ok := p["answers"].(map[string]any); ok {
				return p, true
			}
			if _, ok := p["questions"].([]any); ok {
				return p, true
			}
			for _, key := range [...]string{"text", "output", "content"} {
				if inner, ok := p[key].(string); ok {
					if decoded, ok := asRecord(inner); ok {
						return decoded, true
					}
				}
			}
			return p, true
		case string:
			if decoded, ok := asRecord(p); ok {
				return decoded, true
			}
		}
	}
	return nil, false
}

// ParseQuestions is the questions of a request_user_input tool_input: each record with a non-empty string id, in order, its text
// the question string (an empty one included) or else the header string or else empty. Total: anything unreadable gives none.
func ParseQuestions(toolInput any) []ParsedQuestion {
	out := []ParsedQuestion{}
	input, _ := asRecord(toolInput)
	questions, _ := input["questions"].([]any)
	for _, q := range questions {
		rec, ok := q.(map[string]any)
		if !ok {
			continue
		}
		id, _ := rec["id"].(string)
		if id == "" {
			continue
		}
		question, isString := rec["question"].(string)
		if !isString {
			question, _ = rec["header"].(string)
		}
		out = append(out, ParsedQuestion{id, question})
	}
	return out
}

// ParseAnswers is the answers of a request_user_input tool_response by question id: the string answers of each record that holds an
// answers array. The ids __proto__, constructor and prototype are skipped, as the oracle does against a forged lookup. Total.
func ParseAnswers(toolResponse any) map[string][]string {
	out := map[string][]string{}
	resp, _ := asRecord(toolResponse)
	for qid, val := range answersBody(resp) {
		if qid == "__proto__" || qid == "constructor" || qid == "prototype" {
			continue
		}
		rec, _ := val.(map[string]any)
		list, ok := rec["answers"].([]any)
		if !ok {
			continue
		}
		strs := []string{}
		for _, a := range list {
			if s, ok := a.(string); ok {
				strs = append(strs, s)
			}
		}
		out[qid] = strs
	}
	return out
}

// answersBody is the answers record of resp: its own, even when empty, else the one a wrapper under output, text, content or result
// decodes to.
func answersBody(resp map[string]any) map[string]any {
	if body, ok := resp["answers"].(map[string]any); ok {
		return body
	}
	for _, key := range [...]string{"output", "text", "content", "result"} {
		switch inner := resp[key].(type) {
		case string, []any, map[string]any:
			if decoded, ok := asRecord(inner); ok {
				if body, ok := decoded["answers"].(map[string]any); ok {
					return body
				}
			}
		}
	}
	return nil
}

// eachRow calls fn with every line of the session's ledger that, trimmed as JavaScript trims, is one JSON object. A file that cannot
// be read has no rows.
func eachRow(cwd, sessionID string, fn func(line string, row map[string]any)) {
	data, err := os.ReadFile(ledgerPath(cwd, sessionID))
	if err != nil {
		return
	}
	for _, line := range text.SplitLines(string(data)) {
		line = text.Trim(line)
		if v, ok := parseJSON(line); ok && line != "" {
			if row, ok := v.(map[string]any); ok {
				fn(line, row)
			}
		}
	}
}
