package hook

import (
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson"
	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson/pyjsontest"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// The hook's JSON readers answer as internal/pyjson does with the options the fold gives them,
// over the whole corpus.

var foldedHookOptions = pyjson.LoadOptions{Constants: true, Surrogates: true, RawSurrogates: true, Numbers: pyjson.Int64Numbers}

func foldedHookDecode(raw []byte) (any, error) {
	if _, err := store.DecodeUTF8(raw); err != nil {
		return nil, err
	}
	options := foldedHookOptions
	options.Python = true
	return pyjson.Loads(string(raw), options)
}

func TestFoldHookDecoders(t *testing.T) {
	for _, doc := range pyjsontest.Recorded(t) {
		want, wantErr := Decode([]byte(doc))
		got, err := foldedHookDecode([]byte(doc))
		if !pyjsontest.Errors(wantErr, err) || !pyjsontest.Same(want, got) {
			t.Errorf("Decode(%q) = %#v, %v; folded %#v, %v", doc, want, wantErr, got, err)
		}
		// decodeScanned reads what the scanner accepted, raw WTF-8 surrogates included.
		if pyjson.Error(doc) != "" {
			continue
		}
		want, wantErr = decodeScanned([]byte(doc))
		got, err = pyjson.Loads(doc, foldedHookOptions)
		if !pyjsontest.Errors(wantErr, err) || !pyjsontest.Same(want, got) {
			t.Errorf("decodeScanned(%q) = %#v, %v; folded %#v, %v", doc, want, wantErr, got, err)
		}
	}
}
