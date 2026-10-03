package pyjson

import (
	"reflect"
	"testing"
)

// Text reads a string and nothing else; Map reads a map[string]any and nothing else, an ordered
// Object being a different value than a Go map.
func TestTextAndMapReadOnlyTheirOwnKind(t *testing.T) {
	for _, v := range []any{nil, true, int64(1), 1.5, []any{"a"}, Object{{Key: "a", Value: "b"}}, map[string]any{"a": "b"}, []byte("b")} {
		want := ""
		if s, ok := v.(string); ok {
			want = s
		}
		if got := Text(v); got != want {
			t.Errorf("Text(%#v) = %q, want %q", v, got, want)
		}
	}
	if got := Text("é"); got != "é" {
		t.Errorf("Text(string) = %q", got)
	}
	m := map[string]any{"a": "b"}
	if got := Map(m); !reflect.DeepEqual(got, m) {
		t.Errorf("Map(map) = %v", got)
	}
	for _, v := range []any{nil, "x", int64(1), []any{}, Object{{Key: "a", Value: "b"}}} {
		if got := Map(v); got != nil {
			t.Errorf("Map(%#v) = %v, want nil", v, got)
		}
	}
}
