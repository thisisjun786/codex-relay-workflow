package evidence

import (
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson"
	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson/pyjsontest"
)

// The evidence package's JSON reader, writers and float repr answer as internal/pyjson does with
// the options the fold gives them, over the whole corpus.

func foldedDecode(text string) (value any, err error) {
	value, err = pyjson.Loads(text, pyjson.LoadOptions{Python: true, RangeErrors: true})
	if err != nil {
		return nil, &PythonError{"JSONDecodeError", err.Error()}
	}
	return value, nil
}

func oldDecode(text string) (value any, err error) {
	defer RecoverPython(&err)
	return Decode(text), nil
}

func TestFoldEvidenceDecoder(t *testing.T) {
	for _, doc := range pyjsontest.Recorded(t) {
		want, wantErr := oldDecode(doc)
		got, err := foldedDecode(doc)
		if !pyjsontest.Errors(wantErr, err) || !pyjsontest.Same(want, got) {
			t.Errorf("Decode(%q) = %#v, %v; folded %#v, %v", doc, want, wantErr, got, err)
		}
	}
}

func TestFoldEvidenceWriters(t *testing.T) {
	values := pyjsontest.Decoded(pyjsontest.Recorded(t), oldDecode)
	for _, value := range values {
		for _, compact := range []bool{false, true} {
			for _, sortKeys := range []bool{false, true} {
				for _, ascii := range []bool{false, true} {
					if want, got := Dumps(value, compact, sortKeys, ascii), pyjson.Dumps(value, pyjson.Options{Compact: compact, SortKeys: sortKeys, Unicode: !ascii}); want != got {
						t.Errorf("Dumps(%#v, %v, %v, %v) = %q, folded %q", value, compact, sortKeys, ascii, want, got)
					}
					if compact {
						continue
					}
					if want, got := DumpsIndent(value, 2, sortKeys, ascii), pyjson.Dumps(value, pyjson.Options{Indent: 2, SortKeys: sortKeys, Unicode: !ascii}); want != got {
						t.Errorf("DumpsIndent(%#v, 2, %v, %v) = %q, folded %q", value, sortKeys, ascii, want, got)
					}
				}
			}
		}
	}
	for _, f := range pyjsontest.Floats() {
		if want, got := Float(f), pyjson.Float(f); want != got {
			t.Errorf("Float(%v) = %q, folded %q", f, want, got)
		}
	}
}
