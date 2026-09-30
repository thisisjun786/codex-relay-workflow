package mergeturn

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson"
	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson/pyjsontest"
)

// The merge-turn ledger's readers read what internal/pyjson reads with the options the fold
// gives them, over the whole corpus.

func foldedDecode(raw string) map[string]any {
	value, _ := pyjson.Loads(raw, pyjson.LoadOptions{Map: true, Numbers: pyjson.SpelledNumbers, Trailing: pyjson.TrailingAnything})
	m, _ := value.(map[string]any)
	return m
}

// foldedEnvelope is the object a ledger entry holds, and nothing after it but a closing bracket
// (what json.Decoder.More answers false for), or nil.
func foldedEnvelope(raw string) map[string]any {
	value, err := pyjson.Loads(raw, pyjson.LoadOptions{Map: true, Numbers: pyjson.SpelledNumbers, Trailing: pyjson.TrailingClose})
	m, _ := value.(map[string]any)
	if err != nil {
		return nil
	}
	return m
}

func plainEnvelope(raw string) map[string]any {
	decoder := newDecoder(raw)
	var envelope map[string]any
	if decoder.Decode(&envelope) != nil || envelope == nil || decoder.More() {
		return nil
	}
	return envelope
}

func TestFoldMergeTurnDecoders(t *testing.T) {
	for _, doc := range pyjsontest.Recorded(t) {
		if want, got := decode(doc), foldedDecode(doc); !pyjsontest.Same(want, got) && !(want == nil && got == nil) {
			t.Errorf("decode(%q) = %#v; folded %#v", doc, want, got)
		}
		if want, got := plainEnvelope(doc), foldedEnvelope(doc); !pyjsontest.Same(want, got) && !(want == nil && got == nil) {
			t.Errorf("envelope(%q) = %#v; folded %#v", doc, want, got)
		}
	}
}

func newDecoder(raw string) *json.Decoder {
	decoder := json.NewDecoder(strings.NewReader(raw))
	decoder.UseNumber()
	return decoder
}
