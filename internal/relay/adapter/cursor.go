package adapter

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/pyvalue"
)

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
		return "", &pythonError{"ProgrammingError", "Error binding parameter 3: type '" + pyvalue.TypeName(value) + "' is not supported"}
	}
	var text string
	err := a.store.DB.QueryRowContext(ctx, "SELECT CAST(? AS TEXT)", value).Scan(&text)
	return text, err
}
