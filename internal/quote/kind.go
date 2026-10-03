package quote

import "github.com/thisisjun786/codex-relay-workflow/internal/pyjson"

// Kind names the kind of a decoded JSON value as a message does, with its article: "null",
// "a boolean", "a string", "an array", "an object" and, for every number, "a number". It stands
// where the Python relay's messages named a type (str, dict, NoneType); a message reads
// "..., not " + Kind(v).
func Kind(v any) string {
	switch v.(type) {
	case nil:
		return "null"
	case bool:
		return "a boolean"
	case string:
		return "a string"
	case []any, []string, []map[string]any:
		return "an array"
	case pyjson.Object, map[string]any:
		return "an object"
	}
	return "a number"
}
