//go:build dev

package cxcfuzz

import (
	"math/rand"
	"strconv"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson"
)

// pyjsonTarget is the Python JSON reader/writer (CRW-707, absorbed into CRW-708): pyjson.Loads and
// pyjson.Dumps against the Python standard library json module, reached through
// python3 -m json.tool by testdata/pyjson/shim.mjs. The answer is the process exit status, stdout
// and stderr, so a success compares the re-dumped value and a failure compares the error text.
func pyjsonTarget() Target {
	return Target{
		Name:     "pyjson",
		Generate: pyjsonGenerate,
		Go:       pyjsonGo,
		// The shim is a Node worker like the other targets, but it drives python3 json.tool, so
		// python3 is required on PATH before any case runs.
		Oracle:  Oracle{Command: "node", Shim: shimPath("pyjson"), Root: DefaultOracleRoot, Requires: []string{"python3"}},
		Compare: pyjsonCompare,
	}
}

// pyjsonRequest is what the shim and the Go side read from an input: the JSON document text and the
// json.tool options the case asks for.
type pyjsonRequest struct {
	Text        string
	Indent      int
	Compact     bool
	SortKeys    bool
	EnsureAscii bool
}

func pyjsonRead(input any) pyjsonRequest {
	req := pyjsonRequest{Indent: 4, EnsureAscii: true}
	if v, ok := field(input, "text"); ok {
		req.Text, _ = v.(string)
	}
	if v, ok := field(input, "indent"); ok {
		if n, err := integer(v); err == nil {
			req.Indent = n
		}
	}
	req.Compact = pyjsonFlag(input, "compact")
	req.SortKeys = pyjsonFlag(input, "sortKeys")
	if v, ok := field(input, "ensureAscii"); ok {
		if b, ok := v.(bool); ok {
			req.EnsureAscii = b
		}
	}
	return req
}

func pyjsonFlag(input any, key string) bool {
	v, ok := field(input, key)
	if !ok {
		return false
	}
	b, _ := v.(bool)
	return b
}

// pyjsonGo is the Go side: Loads with the reading the port uses, then Dumps with the options
// json.tool flags map to. A refusal is the error text on stderr with exit 1, the shape the shim
// answers with. Normalize writes a json.Number as json.dumps writes what json.loads made of it,
// which is what json.tool re-dumps.
func pyjsonGo(input any, env Env) (any, error) {
	req := pyjsonRead(input)
	value, err := pyjson.Loads(req.Text, pyjson.LoadOptions{Python: true, Constants: true, Surrogates: true, Numbers: pyjson.PythonNumbers})
	if err != nil {
		return pyjson.Object{{Key: "exit", Value: 1}, {Key: "stdout", Value: ""}, {Key: "stderr", Value: err.Error()}}, nil
	}
	// --compact and an indent are mutually exclusive in json.tool; --compact writes one line, so the
	// indent must be cleared when it is set. json.tool's numeric --indent 0 (a newline per member with
	// no spaces) has no pyjson.Options spelling and is not drawn.
	opts := pyjson.Options{Normalize: true, Compact: req.Compact, SortKeys: req.SortKeys, Unicode: !req.EnsureAscii}
	if !req.Compact {
		opts.Indent = req.Indent
	}
	return pyjson.Object{{Key: "exit", Value: 0}, {Key: "stdout", Value: pyjson.Dumps(value, opts)}, {Key: "stderr", Value: ""}}, nil
}

// pyjsonCompare compares exit, stdout and stderr, which is the oracle whole answer.
func pyjsonCompare(goOut, oracleOut any) Verdict {
	if canonical(goOut) == canonical(oracleOut) {
		return Verdict{Kind: Same}
	}
	return Verdict{Kind: Differ, Detail: "exit, stdout or stderr differs"}
}

// pyjsonDocuments are the boundary documents the issue body names: a lone surrogate escape, a
// surrogate pair, an invalid escape, big integers, an exponent past float range, -0, the constants,
// duplicate keys, a BOM, a control character, trailing data, extra whitespace and a broken document.
var pyjsonDocuments = []string{
	`{"a": "\ud800"}`,
	`{"a": "\ud83d\ude00"}`,
	`{"a": "\uZZZZ"}`,
	`{"a": 123456789012345678901234567890}`,
	`{"a": 1e400}`,
	`{"a": -0}`,
	`{"a": NaN, "b": Infinity, "c": -Infinity}`,
	`{"a": 1, "a": 2}`,
	"\uFEFF" + `{"a": 1}`,
	`{"a": "\u0001"}`,
	`{"a": 1} x`,
	`  {"a": 1}  `,
	`{`,
	`[1, 2, 3]`,
	`"plain"`,
	`null`,
}

// pyjsonIndents are the indent options both sides can express: 4 is json.tool default, 0 is
// --no-indent, a positive N is --indent N. json.tool --indent 0 (a newline per member with no
// spaces) has no pyjson.Options spelling and is not drawn.
var pyjsonIndents = []int{4, 0, 2, 3, 8}

// pyjsonGenerate builds one case: a boundary document or a small generated one, and the options.
func pyjsonGenerate(rng *rand.Rand, size int) any {
	text := pyjsonDocuments[rng.Intn(len(pyjsonDocuments))]
	if rng.Intn(3) == 0 {
		text = pyjsonDocument(rng, 1+rng.Intn(3))
	}
	value := pyjson.Object{{Key: "text", Value: text}}
	switch rng.Intn(4) {
	case 0:
		value = value.Set("indent", pyjsonIndents[rng.Intn(len(pyjsonIndents))])
	case 1:
		value = value.Set("compact", true)
	default:
		// json.tool default indent, no flag: nothing to set.
	}
	if rng.Intn(2) == 0 {
		value = value.Set("sortKeys", true)
	}
	if rng.Intn(3) == 0 {
		value = value.Set("ensureAscii", false)
	}
	return value
}

// pyjsonDocument builds a small valid JSON document: an object of scalars, arrays and nested
// objects, with the values that exercise number and string writing.
func pyjsonDocument(rng *rand.Rand, depth int) string {
	switch rng.Intn(6) {
	case 0:
		return pyjsonScalar(rng)
	case 1, 2:
		keys := []string{"a", "b", "zz", "\u00e9", "k\"q", "n"}
		parts := make([]string, 0, 2)
		for i := 0; i <= rng.Intn(3); i++ {
			parts = append(parts, strconv.Quote(keys[rng.Intn(len(keys))])+": "+pyjsonValue(rng, depth))
		}
		return "{" + strings.Join(parts, ", ") + "}"
	default:
		parts := make([]string, 0, 2)
		for i := 0; i <= rng.Intn(3); i++ {
			parts = append(parts, pyjsonValue(rng, depth))
		}
		return "[" + strings.Join(parts, ", ") + "]"
	}
}

func pyjsonValue(rng *rand.Rand, depth int) string {
	if depth <= 0 || rng.Intn(3) == 0 {
		return pyjsonScalar(rng)
	}
	return pyjsonDocument(rng, depth-1)
}

func pyjsonScalar(rng *rand.Rand) string {
	switch rng.Intn(10) {
	case 0:
		return strconv.Itoa(rng.Intn(2000) - 1000)
	case 1:
		return strconv.FormatFloat(float64(rng.Intn(1000))/8, 'g', -1, 64)
	case 2:
		return "1e" + strconv.Itoa(rng.Intn(30))
	case 3:
		return "true"
	case 4:
		return "false"
	case 5:
		return "null"
	case 6:
		return `"\u00e9"`
	case 7:
		return `"\ud83d\ude00"`
	case 8:
		return `"line\nbreak"`
	default:
		return `"plain"`
	}
}
