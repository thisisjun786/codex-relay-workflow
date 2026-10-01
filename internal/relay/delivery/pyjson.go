package delivery

import (
	"fmt"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson"
)

// Obj is a Python dict in insertion order; the records this package returns keep Python's order.
type Obj = contract.OrderedObject

// F is one field of an Obj.
type F = contract.Field

// get returns the value of key in o, and whether it was present.
func get(o Obj, key string) (any, bool) {
	for _, f := range o {
		if f.Key == key {
			return f.Value, true
		}
	}
	return nil, false
}

// set replaces key in o, or appends it, as a Python dict assignment does.
func set(o Obj, key string, value any) Obj {
	for i, f := range o {
		if f.Key == key {
			o[i].Value = value
			return o
		}
	}
	return append(o, F{Key: key, Value: value})
}

func str(o Obj, key string) string {
	v, _ := get(o, key)
	s, _ := v.(string)
	return s
}

// dumps is Python json.dumps(value) with the default separators and ensure_ascii.
func dumps(value any) string { return pyjson.Dumps(value, pyjson.Options{}) }

// dumpsSorted is json.dumps(value, sort_keys=True).
func dumpsSorted(value any) string { return pyjson.Dumps(value, pyjson.Options{SortKeys: true}) }

// loads reads JSON text as encoding/json reads it, into ordered values (pyjson.Loads): an integer
// is an int64 (past int64, the float it rounds to), any other number a float64, a number past
// float64's range is refused, and nothing but a closing bracket may follow the value.
func loads(text string) (any, error) {
	value, err := pyjson.Loads(text, pyjson.LoadOptions{Numbers: pyjson.Int64FloatNumbers, RangeErrors: true, Trailing: pyjson.TrailingClose})
	if pyjson.ErrTrailing(err) {
		return nil, fmt.Errorf("trailing data")
	}
	return value, err
}

func loadsObj(text string) Obj {
	v, err := loads(text)
	if err != nil {
		return nil
	}
	o, _ := v.(Obj)
	return o
}
