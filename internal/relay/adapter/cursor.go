package adapter

import (
	"context"
	"encoding/json"
	"strings"
)

// Cursors are opaque host values. Truthiness decides exhaustion, not a string
// assertion; within a scan the original value must reach the next request.
func cursorTruthy(value any) bool {
	switch v := value.(type) {
	case nil:
		return false
	case bool:
		return v
	case string:
		return v != ""
	case json.Number:
		if strings.ContainsAny(string(v), ".eE") {
			f, _ := v.Float64() // Python JSON floats may underflow or overflow.
			return f != 0
		}
		return strings.ContainsAny(string(v), "123456789")
	case []any:
		return len(v) != 0
	case map[string]any:
		return len(v) != 0
	default:
		return true
	}
}

// Persistence is a second boundary: Python sqlite3 binds scalars, rejects
// containers and oversized integers, and applies SQLite TEXT affinity. Let
// SQLite perform that conversion rather than inventing a Go string format.
func (a *Adapter) cursorString(ctx context.Context, value any) (string, error) {
	switch v := value.(type) {
	case json.Number:
		if strings.ContainsAny(string(v), ".eE") {
			value, _ = v.Float64()
		} else {
			n, err := v.Int64()
			if err != nil {
				return "", &pythonError{"OverflowError", "Python int too large to convert to SQLite INTEGER"}
			}
			value = n
		}
	case []any, map[string]any:
		return "", &pythonError{"ProgrammingError", "Error binding parameter 3: type '" + pythonType(value) + "' is not supported"}
	}
	var text string
	err := a.store.DB.QueryRowContext(ctx, "SELECT CAST(? AS TEXT)", value).Scan(&text)
	return text, err
}
