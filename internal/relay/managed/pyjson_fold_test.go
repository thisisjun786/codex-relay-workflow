package managed

import (
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson"
	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson/pyjsontest"
)

// The managed package's JSON reader and writers answer as internal/pyjson does with the options
// the fold gives them, over the whole corpus: the request fingerprint hashes compactPythonJSON's
// bytes and authorized_settings stores settingsJSON's.

var foldedObservation = pyjson.LoadOptions{Numbers: pyjson.SpelledNumbers, Repeats: true, Trailing: pyjson.TrailingAnything}

func TestFoldManagedDecoder(t *testing.T) {
	for _, doc := range pyjsontest.Recorded(t) {
		want, wantErr := decodeOrderedJSON([]byte(doc))
		got, err := pyjson.Loads(doc, foldedObservation)
		// The journal detail it reads is JSON the relay wrote; only acceptance is compared.
		if (wantErr == nil) != (err == nil) || !pyjsontest.Same(want, got) {
			t.Errorf("decodeOrderedJSON(%q) = %#v, %v; folded %#v, %v", doc, want, wantErr, got, err)
		}
	}
}

func TestFoldManagedWriters(t *testing.T) {
	values := pyjsontest.Decoded(pyjsontest.Recorded(t), func(doc string) (any, error) {
		return pyjson.Loads(doc, pyjson.LoadOptions{Map: true, Numbers: pyjson.SpelledNumbers})
	})
	values = append(values, pyjsontest.Decoded(pyjsontest.Recorded(t), func(doc string) (any, error) {
		return pyjson.Loads(doc, pyjson.LoadOptions{Map: true, Numbers: pyjson.PythonNumbers})
	})...)
	for _, value := range values {
		want, wantErr := compactPythonJSON(value)
		got, err := pyjson.Encode(value, pyjson.Options{Compact: true, SortKeys: true, Marshal: true, UnescapeHTML: true})
		if !pyjsontest.Errors(wantErr, err) || string(want) != string(got) {
			t.Errorf("compactPythonJSON(%#v) = %q, %v; folded %q, %v", value, want, wantErr, got, err)
		}
		wantText, wantErr := settingsJSON(value)
		got, err = pyjson.Encode(value, pyjson.Options{SortKeys: true, Marshal: true})
		if !pyjsontest.Errors(wantErr, err) || wantText != string(got) {
			t.Errorf("settingsJSON(%#v) = %q, %v; folded %q, %v", value, wantText, wantErr, got, err)
		}
	}
}
