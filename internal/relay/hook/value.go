// Package hook implements the fail-open Stop adapter and its read-only guard.
package hook

import (
	"fmt"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/evidence"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

type Object = contract.OrderedObject
type Field = contract.Field

func object(v any) Object { o, _ := evidence.Object(v); return o }
func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// Decode is json.loads over bytes: strict UTF-8 (its UnicodeDecodeError), then json.loads'
// language and refusals (its JSONDecodeError text), without panic paths. Python's values
// (objects in order, an integer an int64 or a json.Number past it, NaN and the infinities floats, a
// lone surrogate escape kept) are how the hook reads a Stop payload and its records, and how the
// omission reader reads a stored receipt, so the reading is the store's (store.DecodeRecord).
func Decode(raw []byte) (any, error) { return store.DecodeRecord(raw) }

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
