package routing

import (
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson"
	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson/pyjsontest"
)

// routeJSON reads what internal/pyjson reads with the options the fold gives it, over the whole
// corpus; its refusal names json.loads' error whichever reader refused.

func TestFoldRouteJSON(t *testing.T) {
	for _, doc := range pyjsontest.Recorded(t) {
		want, wantErr := routeJSON(doc, "document")
		got, err := pyjson.Loads(doc, pyjson.LoadOptions{Map: true, Numbers: pyjson.SpelledNumbers})
		if (wantErr == nil) != (err == nil) || !pyjsontest.Same(want, got) {
			t.Errorf("routeJSON(%q) = %#v, %v; folded %#v, %v", doc, want, wantErr, got, err)
		}
	}
}
