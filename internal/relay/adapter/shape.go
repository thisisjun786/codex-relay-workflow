package adapter

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
)

type pythonError struct{ kind, message string }

func (e *pythonError) Error() string               { return e.message }
func (e *pythonError) PythonExceptionKind() string { return e.kind }
func pythonType(value any) string {
	switch v := value.(type) {
	case nil:
		return "NoneType"
	case string:
		return "str"
	case bool:
		return "bool"
	case float64:
		return "float"
	case json.Number:
		if strings.ContainsAny(string(v), ".eE") {
			return "float"
		}
		return "int"
	case []any:
		return "list"
	case map[string]any, contract.OrderedObject:
		return "dict"
	default:
		return fmt.Sprintf("%T", value)
	}
}

// subscript is value[key], not dict.get(key): both missing keys and wrong
// container shapes have distinct Python errors on the turn/start receipt path.
func subscript(value any, key string) (any, error) {
	switch v := value.(type) {
	case contract.OrderedObject:
		for _, field := range v {
			if field.Key == key {
				return field.Value, nil
			}
		}
		return nil, &pythonError{"KeyError", "'" + key + "'"}
	case string:
		return nil, &pythonError{"TypeError", "string indices must be integers, not 'str'"}
	case []any:
		return nil, &pythonError{"TypeError", "list indices must be integers or slices, not str"}
	default:
		return nil, &pythonError{"TypeError", "'" + pythonType(value) + "' object is not subscriptable"}
	}
}

func attributeError(value any, name string) error {
	return &pythonError{"AttributeError", fmt.Sprintf("'%s' object has no attribute '%s'", pythonType(value), name)}
}
