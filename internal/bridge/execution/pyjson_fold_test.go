package execution

import (
	"errors"
	"fmt"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson"
	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson/pyjsontest"
)

// The execution policy's reader answers as internal/pyjson does with the options the fold gives
// it, over the whole corpus, the refusal of a repeated key included.

func foldedDecode(raw []byte) (any, error) {
	text, err := pyjson.DecodeBytes(raw)
	if err != nil {
		return nil, &syntaxError{err.Error()}
	}
	if message, duplicate, _ := pyjson.HookedError(text, PolicyDepth); message != "" && !duplicate {
		return nil, &syntaxError{message}
	}
	value, err := pyjson.Loads(text, pyjson.LoadOptions{Constants: true, Surrogates: true, Numbers: pyjson.SpelledNumbers, Unique: true, Deep: true})
	var repeated *pyjson.RepeatedKey
	if errors.As(err, &repeated) {
		return nil, &PolicyError{fmt.Sprintf("duplicate key %s in the execution policy", repr(repeated.Key))}
	}
	return value, err
}

// asObjects is a builder value with every *object a pyjson.Object in its key order.
func asObjects(v any) any {
	switch x := v.(type) {
	case *object:
		out := pyjson.Object{}
		for _, key := range x.keys {
			out = append(out, pyjson.Field{Key: key, Value: asObjects(x.values[key])})
		}
		return out
	case []any:
		for i, item := range x {
			x[i] = asObjects(item)
		}
	}
	return v
}

func TestFoldExecutionDecoder(t *testing.T) {
	docs := append(pyjsontest.Recorded(t), `{"a": 1, "a": 2}`, `{"a": {"b": 1, "b": 2}, "a": 3}`, `{"a": {"b": 1, "c": 2}, "z": [{"q": 1, "q": 1}], "z": 2}`,
		"\xef\xbb\xbf{\"x\": NaN}", "\xff\xfe{\x00}\x00", `{"k": "\ud800"}`)
	for _, doc := range docs {
		want, wantErr := decode([]byte(doc))
		got, err := foldedDecode([]byte(doc))
		if !pyjsontest.Errors(wantErr, err) || !pyjsontest.Same(asObjects(want), got) {
			t.Errorf("decode(%q) = %#v, %v; folded %#v, %v", doc, asObjects(want), wantErr, got, err)
		}
	}
}
