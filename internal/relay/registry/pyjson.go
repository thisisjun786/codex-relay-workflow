package registry

import (
	"errors"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson"
)

// Decoded JSON keeps Python's shapes: an object is a contract.OrderedObject (a dict keeps its
// insertion order), an integer stays a json.Number (Python int), a number with a fraction or an
// exponent is a float64 (Python float), and strings, booleans, nil and []any are themselves.

//lint:ignore ST1005 json/decoder.py:340 caller-visible message kept byte-identical to Python
var errTrailing = errors.New("Extra data")

// decodeJSON is json.loads as the registry reads a stored document: encoding/json's reading
// (pyjson.Loads), a refusal in its words and "Extra data" for what follows the value.
func decodeJSON(data []byte) (any, error) {
	value, err := pyjson.Loads(string(data), pyjson.LoadOptions{})
	if pyjson.ErrTrailing(err) {
		return nil, errTrailing
	}
	return value, err
}

func dropField(object contract.OrderedObject, key string) contract.OrderedObject {
	out := contract.OrderedObject{}
	for _, field := range object {
		if field.Key != key {
			out = append(out, field)
		}
	}
	return out
}

func copyObject(object contract.OrderedObject) contract.OrderedObject {
	return append(contract.OrderedObject{}, object...)
}

// canonical is settings._canonical: json.dumps(value, sort_keys=True), where 0 and false differ.
func canonical(value any) string { return pyjson.Dumps(value, pyjson.Options{SortKeys: true}) }

// textList is settings._text_list: a list whose every member is text.
func textList(value any) bool {
	list, ok := value.([]any)
	if !ok {
		return false
	}
	for _, item := range list {
		if _, ok := item.(string); !ok {
			return false
		}
	}
	return true
}

func anyStrings(values []string) []any {
	out := make([]any, len(values))
	for i, v := range values {
		out[i] = v
	}
	return out
}

// jsonEqual is Python == over decoded JSON, which is what the resume restatement compares.
func jsonEqual(a, b any) bool { return canonical(a) == canonical(b) }
