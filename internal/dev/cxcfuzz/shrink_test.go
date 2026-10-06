//go:build dev

package cxcfuzz

import (
	"fmt"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson"
)

// The shrinker keeps a candidate only while the verdict survives, so a value with one decisive
// field reduces to that field alone.
func TestShrinkKeepsOnlyWhatTheVerdictNeeds(t *testing.T) {
	input := pyjson.Object{{Key: "keep", Value: "x"}, {Key: "drop", Value: "y"}, {Key: "also", Value: "z"}}
	keep := func(candidate any) bool {
		object, ok := candidate.(pyjson.Object)
		if !ok {
			return false
		}
		value, found := object.Lookup("keep")
		return found && value == "x"
	}
	shrunk, attempts := Shrink(input, 500, keep)
	if attempts > 500 {
		t.Fatalf("attempts %d", attempts)
	}
	if got := canonical(shrunk); got != `{"keep": "x"}` {
		t.Fatalf("shrunk %s", got)
	}
}

// The attempt cap bounds a shrink that never converges.
func TestShrinkStopsAtTheAttemptCap(t *testing.T) {
	big := pyjson.Object{}
	for i := 0; i < 200; i++ {
		big = big.Set(fmt.Sprintf("k%03d", i), "v")
	}
	shrunk, attempts := Shrink(big, 5, func(any) bool { return true })
	if attempts != 5 {
		t.Fatalf("attempts %d, want 5", attempts)
	}
	if len(shrunk.(pyjson.Object)) != 195 {
		t.Fatalf("shrunk to %d fields, want 195", len(shrunk.(pyjson.Object)))
	}
}

// The same input and verdict shrink the same way.
func TestShrinkIsDeterministic(t *testing.T) {
	input := pyjson.Object{{Key: "a", Value: []any{"x", "y", "z"}}, {Key: "b", Value: "keep"}}
	keep := func(candidate any) bool {
		object, ok := candidate.(pyjson.Object)
		if !ok {
			return false
		}
		_, found := object.Lookup("b")
		return found
	}
	one, _ := Shrink(input, 500, keep)
	two, _ := Shrink(input, 500, keep)
	if canonical(one) != canonical(two) {
		t.Fatalf("%s != %s", canonical(one), canonical(two))
	}
}
