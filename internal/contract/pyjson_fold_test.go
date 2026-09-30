package contract

import (
	"bytes"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson"
	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson/pyjsontest"
)

// Emit writes what internal/pyjson's Encode writes with indent=2, and fails where it fails, over
// the whole corpus.

func foldedEmit(value any) ([]byte, error) {
	data, err := pyjson.Encode(value, pyjson.Options{Indent: 2})
	if err != nil {
		return nil, err
	}
	return append(data, '\n'), nil
}

// emitTypes are the values Emit is given: what the relay's readers build and its own records.
var emitTypes = []string{"pyjson.Object", "[]interface {}", "string", "bool", "nil", "int64", "int", "float64", "json.Number"}

func TestFoldEmit(t *testing.T) {
	values := pyjsontest.Decoded(pyjsontest.Recorded(t), func(doc string) (any, error) {
		return pyjson.Loads(doc, pyjson.LoadOptions{Python: true, Constants: true, Surrogates: true, Numbers: pyjson.Int64Numbers})
	})
	values = append(values, pyjsontest.Decoded(pyjsontest.Recorded(t), func(doc string) (any, error) {
		return pyjson.Loads(doc, pyjson.LoadOptions{Numbers: pyjson.SpelledNumbers})
	})...)
	for _, value := range values {
		if !pyjsontest.Within(value, emitTypes...) {
			continue
		}
		var want bytes.Buffer
		wantErr := Emit(&want, value)
		got, err := foldedEmit(value)
		if (wantErr == nil) != (err == nil) || wantErr == nil && want.String() != string(got) {
			t.Errorf("Emit(%#v) = %q, %v; folded %q, %v", value, want.String(), wantErr, got, err)
		}
	}
}
