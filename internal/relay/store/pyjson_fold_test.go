package store

import (
	"encoding/json"
	"math"
	"testing"
	"unicode/utf8"

	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson"
	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson/pyjsontest"
)

// The store's JSON readers, writer and float repr answer as internal/pyjson does with the options
// the fold gives them, over the whole corpus.

// plain is a jsonValue as the Go values pyjson.Loads gives.
func plain(v jsonValue) any {
	switch v.kind {
	case jsonObject:
		object := pyjson.Object{}
		for _, f := range v.object {
			object = append(object, pyjson.Field{Key: f.key, Value: plain(f.value)})
		}
		return object
	case jsonArray:
		items := []any{}
		for _, item := range v.array {
			items = append(items, plain(item))
		}
		return items
	}
	return v.scalar
}

// valueOf is the jsonValue a pyjson value is.
func valueOf(v any) jsonValue {
	switch x := v.(type) {
	case pyjson.Object:
		out := jsonValue{kind: jsonObject, object: []jsonField{}}
		for _, f := range x {
			out.object = append(out.object, jsonField{key: f.Key, value: valueOf(f.Value)})
		}
		return out
	case []any:
		out := jsonValue{kind: jsonArray, array: []jsonValue{}}
		for _, item := range x {
			out.array = append(out.array, valueOf(item))
		}
		return out
	}
	return jsonValue{kind: jsonScalar, scalar: v}
}

var foldedOrdered = pyjson.LoadOptions{Numbers: pyjson.SpelledNumbers}

func TestFoldStoreDecoders(t *testing.T) {
	for _, doc := range pyjsontest.Recorded(t) {
		old, oldErr := decodeOrdered([]byte(doc))
		got, err := pyjson.Loads(doc, foldedOrdered)
		if (oldErr == nil) != (err == nil) || oldErr == nil && !pyjsontest.Same(plain(old), got) {
			t.Errorf("decodeOrdered(%q) = %#v, %v; folded %#v, %v", doc, plain(old), oldErr, got, err)
		}
		want, wantErr := LoadsJSON([]byte(doc))
		got, err = pyjson.Loads(doc, pyjson.LoadOptions{Python: true, Constants: true})
		if !pyjsontest.Errors(wantErr, err) || !pyjsontest.Same(want, got) {
			t.Errorf("LoadsJSON(%q) = %#v, %v; folded %#v, %v", doc, want, wantErr, got, err)
		}
		// frozenRecords reads the manifest as strict UTF-8 and has PythonJSONError accept it first.
		if PythonJSONError(doc) != "" || !utf8.ValidString(doc) {
			continue
		}
		want, wantErr = decodePythonJSON(doc)
		got, err = pyjson.Loads(doc, pyjson.LoadOptions{Constants: true, Surrogates: true, Numbers: pyjson.BigNumbers, Deep: true})
		if !pyjsontest.Errors(wantErr, err) || !pyjsontest.Same(want, got) {
			t.Errorf("decodePythonJSON(%q) = %#v, %v; folded %#v, %v", doc, want, wantErr, got, err)
		}
	}
}

func TestFoldStoreWriters(t *testing.T) {
	values := pyjsontest.Decoded(pyjsontest.Recorded(t), func(doc string) (any, error) { return pyjson.Loads(doc, foldedOrdered) })
	for _, value := range values {
		if !ordered(value) {
			continue
		}
		want, wantErr := pythonDumps(valueOf(value))
		got, err := pyjson.Encode(value, pyjson.Options{Normalize: true})
		if (wantErr == nil) != (err == nil) || want != string(got) {
			t.Errorf("pythonDumps(%#v) = %q, %v; folded %q, %v", value, want, wantErr, got, err)
		}
	}
	for _, f := range pyjsontest.Floats() {
		if math.IsNaN(f) {
			continue
		}
		want := pythonFloat(f)
		if got := pyjson.Dumps(f, pyjson.Options{}); want != got {
			t.Errorf("pythonFloat(%v) = %q, folded %q", f, want, got)
		}
	}
}

// ordered reports whether v holds only what decodeOrdered builds: Objects, []any and the
// scalars json.Decoder gives (string, bool, nil, json.Number).
func ordered(v any) bool {
	switch x := v.(type) {
	case pyjson.Object:
		for _, f := range x {
			if !ordered(f.Value) {
				return false
			}
		}
		return true
	case []any:
		for _, item := range x {
			if !ordered(item) {
				return false
			}
		}
		return true
	case string, bool, nil, json.Number:
		return true
	}
	return false
}
