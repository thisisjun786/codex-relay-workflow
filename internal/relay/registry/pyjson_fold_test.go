package registry

import (
	"math"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson"
	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson/pyjsontest"
)

// The registry's JSON reader, writer and float repr answer as internal/pyjson does with the
// options the fold gives them, over the whole corpus.

func foldedDecodeJSON(data []byte) (any, error) {
	value, err := pyjson.Loads(string(data), pyjson.LoadOptions{})
	if pyjson.ErrTrailing(err) {
		return nil, errTrailing
	}
	return value, err
}

func foldedDecodeJSONText(raw string) (any, error) {
	return pyjson.Loads(raw, pyjson.LoadOptions{Python: true})
}

func TestFoldRegistryDecoders(t *testing.T) {
	for _, doc := range pyjsontest.Recorded(t) {
		want, wantErr := decodeJSON([]byte(doc))
		got, err := foldedDecodeJSON([]byte(doc))
		if !pyjsontest.Errors(wantErr, err) || !pyjsontest.Same(want, got) {
			t.Errorf("decodeJSON(%q) = %#v, %v; folded %#v, %v", doc, want, wantErr, got, err)
		}
		want, wantErr = DecodeJSON(doc)
		got, err = foldedDecodeJSONText(doc)
		if !pyjsontest.Errors(wantErr, err) || !pyjsontest.Same(want, got) {
			t.Errorf("DecodeJSON(%q) = %#v, %v; folded %#v, %v", doc, want, wantErr, got, err)
		}
	}
}

var registryTypes = []string{"pyjson.Object", "[]interface {}", "[]string", "string", "bool", "nil", "int64", "int", "float64", "json.Number", "*big.Int"}

func TestFoldRegistryWriters(t *testing.T) {
	values := pyjsontest.Decoded(pyjsontest.Recorded(t), func(doc string) (any, error) { return decodeJSON([]byte(doc)) })
	for _, value := range values {
		// The registry writes what its reader builds and Objects, lists and scalars of its own;
		// never a map, which its writer would spell as fmt does.
		if !pyjsontest.Within(value, registryTypes...) {
			continue
		}
		for _, sorted := range []bool{false, true} {
			if want, got := pyDumps(value, sorted), pyjson.Dumps(value, pyjson.Options{SortKeys: sorted}); want != got {
				t.Errorf("pyDumps(%#v, %v) = %q, folded %q", value, sorted, want, got)
			}
		}
	}
	for _, f := range pyjsontest.Floats() {
		if math.IsNaN(f) || math.IsInf(f, 0) {
			continue
		}
		if want, got := pyFloat(f), pyjson.Float(f); want != got {
			t.Errorf("pyFloat(%v) = %q, folded %q", f, want, got)
		}
	}
}
