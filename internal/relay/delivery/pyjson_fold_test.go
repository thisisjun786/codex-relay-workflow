package delivery

import (
	"errors"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson"
	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson/pyjsontest"
)

// The delivery package's JSON reader, writers and float spelling answer as internal/pyjson does
// with the options the fold gives them, over the whole corpus.

func foldedLoads(text string) (any, error) {
	value, err := pyjson.Loads(text, pyjson.LoadOptions{Numbers: pyjson.Int64FloatNumbers, RangeErrors: true, Trailing: pyjson.TrailingClose})
	if pyjson.ErrTrailing(err) {
		return nil, errors.New("trailing data")
	}
	return value, err
}

func TestFoldDeliveryDecoder(t *testing.T) {
	for _, doc := range pyjsontest.Recorded(t) {
		want, wantErr := loads(doc)
		got, err := foldedLoads(doc)
		if !pyjsontest.Errors(wantErr, err) || !pyjsontest.Same(want, got) {
			t.Errorf("loads(%q) = %#v, %v; folded %#v, %v", doc, want, wantErr, got, err)
		}
	}
}

// deliveryTypes are the Go values delivery's writers are given: what its reader builds, and the
// ints, []string and json.Number its records carry.
var deliveryTypes = []string{"pyjson.Object", "[]interface {}", "string", "bool", "nil", "int64", "int", "float64", "json.Number", "[]string"}

func TestFoldDeliveryWriters(t *testing.T) {
	values := pyjsontest.Decoded(pyjsontest.Recorded(t), loads)
	for _, value := range values {
		if !pyjsontest.Within(value, deliveryTypes...) {
			continue
		}
		if want, got := dumps(value), pyjson.Dumps(value, pyjson.Options{}); want != got {
			t.Errorf("dumps(%#v) = %q, folded %q", value, want, got)
		}
		if want, got := dumpsSorted(value), pyjson.Dumps(value, pyjson.Options{SortKeys: true}); want != got {
			t.Errorf("dumpsSorted(%#v) = %q, folded %q", value, want, got)
		}
		// canonical and jsonCompact write a []string inside a record through dumps, with its
		// ", " separators; no fact, marker or criterion carries one.
		if !pyjsontest.Within(value, "pyjson.Object", "[]interface {}", "string", "bool", "nil", "int64", "int", "float64", "json.Number") {
			continue
		}
		if want, got := canonical(value), pyjson.Dumps(value, pyjson.Options{Compact: true, SortKeys: true}); want != got {
			t.Errorf("canonical(%#v) = %q, folded %q", value, want, got)
		}
		if want, got := jsonCompact(value), pyjson.Dumps(value, pyjson.Options{Compact: true, SortKeys: true, Unicode: true, Bytes: pyjson.ReplacedAll}); want != got {
			t.Errorf("jsonCompact(%#v) = %q, folded %q", value, want, got)
		}
	}
	for _, f := range pyjsontest.Floats() {
		if want, got := pyFloat(f), pyjson.Dumps(f, pyjson.Options{}); want != got {
			t.Errorf("pyFloat(%v) = %q, folded %q", f, want, got)
		}
	}
}
