package service

import (
	"fmt"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson"
	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson/pyjsontest"
)

// The service record reader answers as internal/pyjson does with the options the fold gives it,
// over the whole corpus; every caller reads a refusal as "unreadable", so only acceptance counts.

func foldedParse(raw []byte) (Object, error) {
	value, err := pyjson.Loads(string(raw), pyjson.LoadOptions{Numbers: pyjson.SpelledNumbers, Repeats: true})
	if err != nil {
		return nil, err
	}
	o, ok := value.(Object)
	if !ok {
		return nil, fmt.Errorf("record is not an object")
	}
	return o, nil
}

func TestFoldServiceDecoder(t *testing.T) {
	for _, doc := range pyjsontest.Recorded(t) {
		want, wantErr := parse([]byte(doc))
		got, err := foldedParse([]byte(doc))
		if (wantErr == nil) != (err == nil) || !pyjsontest.Same(want, got) {
			t.Errorf("parse(%q) = %#v, %v; folded %#v, %v", doc, want, wantErr, got, err)
		}
	}
}
