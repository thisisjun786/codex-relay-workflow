//go:build dev

package pyload

import (
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson"
	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson/pyjsontest"
)

// Loads reads what internal/pyjson reads with the options the fold gives it, over the whole corpus.

func TestFoldPyload(t *testing.T) {
	for _, doc := range pyjsontest.Recorded(t) {
		if pyjson.Error(doc) != "" {
			continue
		}
		want, wantErr := Loads([]byte(doc))
		if wantErr != nil {
			continue // not UTF-8
		}
		got, err := pyjson.Loads(doc, pyjson.LoadOptions{Constants: true, Surrogates: true, Numbers: pyjson.Int64Numbers, Deep: true})
		if err != nil || !pyjsontest.Same(want, got) {
			t.Errorf("Loads(%q) = %#v; folded %#v, %v", doc, want, got, err)
		}
	}
}
