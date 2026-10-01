package delivery

import (
	"math"
	"testing"
)

// Delivery's text spells a float that is not finite as JSON does, as it did before the shared
// pyvalue existed; every other value reads as pyvalue.Str.
func TestDeliveryTextKeepsTheJSONSpellingOfNonFiniteFloats(t *testing.T) {
	for _, c := range []struct {
		value any
		want  string
	}{
		{math.NaN(), "NaN"}, {math.Inf(1), "Infinity"}, {math.Inf(-1), "-Infinity"},
		{1.5, "1.5"}, {1e16, "1e+16"}, {int64(3), "3"}, {nil, "None"}, {true, "True"}, {"text", "text"},
	} {
		if got := pyStr(c.value); got != c.want {
			t.Errorf("pyStr(%v) = %q, want %q", c.value, got, c.want)
		}
		if got := inline(c.value); got != c.want {
			t.Errorf("inline(%v) = %q, want %q", c.value, got, c.want)
		}
	}
}

// A float that is not finite keeps the JSON spelling inside a list or object too, and every
// finite value reads as pyvalue.Repr does.
func TestDeliveryReprKeepsTheJSONSpellingOfNonFiniteFloatsAtAnyDepth(t *testing.T) {
	value := Obj{{Key: "a", Value: []any{math.NaN(), 1.5, "x", nil}}, {Key: "b", Value: math.Inf(-1)}}
	if got, want := pyReprValue(value), "{'a': [NaN, 1.5, 'x', None], 'b': -Infinity}"; got != want {
		t.Fatalf("pyReprValue = %q, want %q", got, want)
	}
	if got, want := pyStr(value), "{'a': [NaN, 1.5, 'x', None], 'b': -Infinity}"; got != want {
		t.Fatalf("pyStr = %q, want %q", got, want)
	}
	finite := Obj{{Key: "k", Value: []any{int64(1), 2.25, true}}}
	if got, want := pyReprValue(finite), "{'k': [1, 2.25, True]}"; got != want {
		t.Fatalf("pyReprValue = %q, want %q", got, want)
	}
}
