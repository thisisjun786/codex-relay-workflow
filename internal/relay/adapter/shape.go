package adapter

import (
	"fmt"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
	"github.com/thisisjun786/codex-relay-workflow/internal/pyvalue"
)

type pythonError struct{ kind, message string }

func (e *pythonError) Error() string               { return e.message }
func (e *pythonError) PythonExceptionKind() string { return e.kind }

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
		return nil, &pythonError{"TypeError", "'" + pyvalue.TypeName(value) + "' object is not subscriptable"}
	}
}

func attributeError(value any, name string) error {
	return &pythonError{"AttributeError", fmt.Sprintf("'%s' object has no attribute '%s'", pyvalue.TypeName(value), name)}
}
