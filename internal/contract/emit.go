package contract

import (
	"fmt"
	"io"

	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson"
)

// Field is one entry in a Python insertion-ordered JSON object (pyjson.Field).
type Field = pyjson.Field

// OrderedObject preserves field order at every object nesting level (pyjson.Object).
type OrderedObject = pyjson.Object

// Result is the JSON envelope returned by a relay command.
type Result = OrderedObject

// Emit writes Python json.dumps(..., indent=2) output and its trailing newline (pyjson.Encode).
// Objects must be OrderedObject rather than maps: Go maps cannot represent insertion order.
func Emit(w io.Writer, value any) error {
	data, err := pyjson.Encode(value, pyjson.Options{Indent: 2})
	if err != nil {
		return fmt.Errorf("encode result: %w", err)
	}
	if _, err := w.Write(append(data, '\n')); err != nil {
		return fmt.Errorf("write result: %w", err)
	}
	return nil
}
