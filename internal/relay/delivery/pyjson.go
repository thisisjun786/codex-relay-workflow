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
