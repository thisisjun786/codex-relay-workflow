//go:build dev

// Package pyload is json.loads for the development judges (crw-dev stop-events and
// crw-dev trial-ledger): the value hook.Decode builds from a record (ordered objects, int64 or
// json.Number integers, float64, WTF-8 strings), without encoding/json's nesting limit of 10000.
//
// CPython 3.14, the interpreter the judges answer for, has no nesting limit of its own: its C
// scanner recurses until the thread's stack runs out and raises RecursionError there. Loads
// models that edge as Nesting and returns the RecursionError past it, so a judge reads what
// Python reads and fails where Python fails rather than at encoding/json's 10000. The real edge
// moves with the stack and the container kind, which no model reproduces; docs/port/known-defects.md
// names the difference.
package pyload

import (
	"encoding/json"
	"errors"
	"math"
	"strconv"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/ledger"
	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/evidence"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// Nesting is how many containers deep a judge reads a record. CPython 3.14.4 on the default
// 8 MiB main-thread stack stopped at 57,930 (stop_events.py, a journal row), 57,929 and 57,955
// (trial_startup.py ledger, a ledger line and the start record): where exactly moves by tens with
// whatever else is on the stack. The model stays just below every edge measured, so a judge
// never reads a record the interpreter could not; between Nesting and the interpreter's edge it
// raises where Python still reads.
const Nesting = 57900

// Loads is json.loads(raw.decode("utf-8")): the decoded value, the ValueError Python raises
// (its message), or past Nesting the RecursionError, as an *evidence.PythonError a judge raises
// on, because neither reader it ports catches one.
func Loads(raw []byte) (any, error) {
	if _, err := store.DecodeUTF8(raw); err != nil {
		return nil, err
	}
	text := string(raw)
	if message, recursion := pyjson.ErrorWithLimit(text, Nesting); recursion {
		return nil, &evidence.PythonError{Class: "RecursionError", Detail: message}
	} else if message != "" {
		return nil, errors.New(message)
	}
	p := parser{s: text}
	return p.value()
}

// Recursion is the RecursionError Loads returned, if it returned one.
func Recursion(err error) (*evidence.PythonError, bool) {
	var python *evidence.PythonError
	if errors.As(err, &python) && python.Class == "RecursionError" {
		return python, true
	}
	return nil, false
}

// parser walks a document pyjson has already accepted, so it looks only for where each value
// ends; every syntax question was answered, with Python's message, before it starts.
type parser struct {
	s string
	i int
}

func (p *parser) space() {
	for p.i < len(p.s) && strings.IndexByte(" \t\n\r", p.s[p.i]) >= 0 {
		p.i++
	}
}

func (p *parser) value() (any, error) {
	p.space()
	switch c := p.s[p.i]; {
	case c == '{':
		p.i++
		o := contract.OrderedObject{}
		p.space()
		if p.s[p.i] == '}' {
			p.i++
			return o, nil
		}
		for {
			p.space()
			key, err := p.str()
			if err != nil {
				return nil, err
			}
			p.space()
			p.i++ // ':'
			item, err := p.value()
			if err != nil {
				return nil, err
			}
			o = set(o, key, item)
			p.space()
			if p.s[p.i] == ',' {
				p.i++
				continue
			}
			p.i++ // '}'
			return o, nil
		}
	case c == '[':
		p.i++
		a := []any{}
		p.space()
		if p.s[p.i] == ']' {
			p.i++
			return a, nil
		}
		for {
			item, err := p.value()
			if err != nil {
				return nil, err
			}
			a = append(a, item)
			p.space()
			if p.s[p.i] == ',' {
				p.i++
				continue
			}
			p.i++ // ']'
			return a, nil
		}
	case c == '"':
		return p.str()
	case strings.HasPrefix(p.s[p.i:], "null"):
		p.i += 4
		return nil, nil
	case strings.HasPrefix(p.s[p.i:], "true"):
		p.i += 4
		return true, nil
	case strings.HasPrefix(p.s[p.i:], "false"):
		p.i += 5
		return false, nil
	case strings.HasPrefix(p.s[p.i:], "NaN"):
		p.i += 3
		return math.NaN(), nil
	case strings.HasPrefix(p.s[p.i:], "Infinity"):
		p.i += 8
		return math.Inf(1), nil
	case strings.HasPrefix(p.s[p.i:], "-Infinity"):
		p.i += 9
		return math.Inf(-1), nil
	}
	start := p.i
	for p.i < len(p.s) && strings.IndexByte("0123456789+-.eE", p.s[p.i]) >= 0 {
		p.i++
	}
	spelled := p.s[start:p.i]
	if strings.ContainsAny(spelled, ".eE") {
		n, err := strconv.ParseFloat(spelled, 64)
		if math.IsInf(n, 0) {
			return n, nil // Python's float() overflows to inf rather than refusing.
		}
		return n, err
	}
	if n, err := strconv.ParseInt(spelled, 10, 64); err == nil {
		return n, nil
	}
	return json.Number(spelled), nil // an int past int64, as hook.Decode leaves one
}

// str is one string, decoded by the bridge ledger's decoder as hook.Decode decodes it: a lone
// surrogate escape stays that code point (WTF-8) rather than becoming U+FFFD.
func (p *parser) str() (string, error) {
	start := p.i
	p.i++
	for p.s[p.i] != '"' {
		if p.s[p.i] == '\\' {
			p.i++
		}
		p.i++
	}
	p.i++
	decoded, err := ledger.DecodeJSON([]byte(p.s[start:p.i]))
	if err != nil {
		return "", err
	}
	text, ok := decoded.(string)
	if !ok {
		return "", errors.New("a JSON string did not decode to text")
	}
	return text, nil
}

// set is a dict assignment: a repeated key keeps its first position and takes the last value.
func set(o contract.OrderedObject, key string, value any) contract.OrderedObject {
	for i := range o {
		if o[i].Key == key {
			o[i].Value = value
			return o
		}
	}
	return append(o, contract.Field{Key: key, Value: value})
}
