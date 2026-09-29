package store

import (
	"unicode/utf8"

	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson"
)

// PythonJSONError returns json.loads's JSONDecodeError text for a document it refuses, or ""
// when Python would accept it (pyjson.Error).
func PythonJSONError(doc string) string { return pyjson.Error(doc) }

// PythonJSONErrorWithLimit also models the C JSON scanner's container recursion budget
// (pyjson.ErrorWithLimit).
func PythonJSONErrorWithLimit(doc string, maxDepth int) (message string, recursion bool) {
	return pyjson.ErrorWithLimit(doc, maxDepth)
}

// ValidUTF8 guards the character positions above; Python reads the file as UTF-8 first.
func ValidUTF8(data []byte) bool { return utf8.Valid(data) }

// DecodeUTF8 is Python's strict UTF-8 text-file decoding (pyjson.DecodeUTF8).
func DecodeUTF8(data []byte) (string, error) { return pyjson.DecodeUTF8(data) }
