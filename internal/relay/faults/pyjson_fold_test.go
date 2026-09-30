package faults

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson"
	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson/pyjsontest"
)

// The fault ledger's JSON reader, writer and float spelling answer as internal/pyjson does with
// the options the fold gives them, over the whole corpus: fault ids hash dumps' bytes.

var foldedFaultsLoads = pyjson.LoadOptions{Map: true, Numbers: pyjson.SpelledNumbers, Trailing: pyjson.TrailingAnything}

// sameError also compares where a *json.SyntaxError stands, which f2JSON reads.
func sameError(a, b error) bool {
	var x, y *json.SyntaxError
	if errors.As(a, &x) != errors.As(b, &y) || x != nil && x.Offset != y.Offset {
		return false
	}
	return pyjsontest.Errors(a, b)
}

func TestFoldFaultsDecoder(t *testing.T) {
	for _, doc := range pyjsontest.Recorded(t) {
		want, wantErr := loads(doc)
		got, err := pyjson.Loads(doc, foldedFaultsLoads)
		if !sameError(wantErr, err) || !pyjsontest.Same(want, got) {
			t.Errorf("loads(%q) = %#v, %v; folded %#v, %v", doc, want, wantErr, got, err)
		}
	}
}

// faultsTypes are the Go values the fault ledger's writer is given.
var faultsTypes = []string{"map[string]interface {}", "[]interface {}", "[]string", "string", "bool", "nil", "int64", "int", "float64", "json.Number"}

func TestFoldFaultsWriter(t *testing.T) {
	values := pyjsontest.Decoded(pyjsontest.Recorded(t), loads)
	for _, value := range values {
		if !pyjsontest.Within(value, faultsTypes...) {
			continue
		}
		for _, compact := range []bool{false, true} {
			if want, got := dumps(value, compact), pyjson.Dumps(value, pyjson.Options{Compact: compact, SortKeys: true, Unicode: true, Bytes: pyjson.ReplacedAll}); want != got {
				t.Errorf("dumps(%#v, %v) = %q, folded %q", value, compact, want, got)
			}
		}
	}
	for _, f := range pyjsontest.Floats() {
		if want, got := pyFloat(f), pyjson.Dumps(f, pyjson.Options{}); want != got {
			t.Errorf("pyFloat(%v) = %q, folded %q", f, want, got)
		}
	}
}
