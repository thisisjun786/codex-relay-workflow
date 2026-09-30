// Package hook implements the fail-open Stop adapter and its read-only guard.
package hook

import (
	"fmt"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/evidence"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

type Object = contract.OrderedObject
type Field = contract.Field

func get(o Object, key string) any { return evidence.Get(o, key) }
func object(v any) Object          { o, _ := evidence.Object(v); return o }
func text(v any) string            { s, _ := v.(string); return s }
func set(o Object, key string, v any) Object {
	for i := range o {
		if o[i].Key == key {
			o[i].Value = v
			return o
		}
	}
	return append(o, Field{Key: key, Value: v})
}
func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// hookValues is how the hook reads a Stop payload and its records: Python's values (pyjson.Loads),
// objects in order, an integer an int64 (a json.Number past it), NaN and the infinities floats,
// and a lone surrogate, escaped or in the WTF-8 bytes a frame decoded with surrogatepass holds it
// in, kept.
var hookValues = pyjson.LoadOptions{Constants: true, Surrogates: true, RawSurrogates: true, Numbers: pyjson.Int64Numbers}

// Decode is json.loads over bytes: strict UTF-8 (its UnicodeDecodeError), then json.loads'
// language and refusals (its JSONDecodeError text), without panic paths.
func Decode(raw []byte) (any, error) {
	if _, err := store.DecodeUTF8(raw); err != nil {
		return nil, err
	}
	options := hookValues
	options.Python = true
	return pyjson.Loads(string(raw), options)
}

// decodeScanned is Decode past its checks: raw is text Python's JSON scanner accepted.
func decodeScanned(raw []byte) (any, error) { return pyjson.Loads(string(raw), hookValues) }

func decodeObject(raw []byte) (Object, error) {
	v, err := Decode(raw)
	if err != nil {
		return nil, err
	}
	o, ok := evidence.Object(v)
	if !ok {
		return nil, fmt.Errorf("the Stop payload must be a JSON object")
	}
	return o, nil
}
