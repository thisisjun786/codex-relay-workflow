//go:build dev

package cxcfuzz

import (
	"encoding/json"
	"sort"

	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson"
)

// Shrink reduces input one step at a time while keep reports the same verdict for the candidate,
// and returns the smallest value it reached with the number of candidate evaluations it spent.
// The order is fixed, so the same input and verdict shrink the same way.
func Shrink(input any, attempts int, keep func(any) bool) (any, int) {
	current := input
	used := 0
	for {
		progressed := false
		for _, candidate := range reduce(current) {
			if used >= attempts {
				return current, used
			}
			used++
			if keep(candidate) {
				current = candidate
				progressed = true
				break
			}
		}
		if !progressed {
			return current, used
		}
	}
}

// reduce is one step's candidates for a value, in a fixed order: a container first loses its
// members one at a time, then each member is reduced in place; a string loses its halves and then
// all of it; a number becomes zero; a boolean becomes false. A generator may build its input as a
// pyjson.Object (what decode answers) or as a plain map; a plain map is reduced as an object in
// sorted key order, so the candidates stay deterministic.
func reduce(value any) []any {
	switch v := value.(type) {
	case map[string]any:
		keys := make([]string, 0, len(v))
		for key := range v {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		fields := make(pyjson.Object, 0, len(keys))
		for _, key := range keys {
			fields = append(fields, pyjson.Field{Key: key, Value: v[key]})
		}
		return reduce(fields)
	case pyjson.Object:
		var out []any
		for i := range v {
			smaller := make(pyjson.Object, 0, len(v)-1)
			smaller = append(smaller, v[:i]...)
			smaller = append(smaller, v[i+1:]...)
			out = append(out, smaller)
		}
		for i := range v {
			for _, inner := range reduce(v[i].Value) {
				// Object.Set writes into the receiver, so the candidate is a copy: a step must
				// never change the value it is reducing.
				next := make(pyjson.Object, len(v))
				copy(next, v)
				out = append(out, next.Set(v[i].Key, inner))
			}
		}
		return out
	case []any:
		var out []any
		for i := range v {
			smaller := make([]any, 0, len(v)-1)
			smaller = append(smaller, v[:i]...)
			smaller = append(smaller, v[i+1:]...)
			out = append(out, smaller)
		}
		for i := range v {
			for _, inner := range reduce(v[i]) {
				next := append([]any{}, v...)
				next[i] = inner
				out = append(out, next)
			}
		}
		return out
	case string:
		if v == "" {
			return nil
		}
		// The halves first, then one character off each end, then nothing: the issue's trimming
		// at both ends and one character at a time.
		return []any{v[:len(v)/2], v[1:], v[:len(v)-1], ""}
	case bool:
		if v {
			return []any{false}
		}
		return nil
	case int:
		if v != 0 {
			return []any{0}
		}
		return nil
	case int64:
		if v != 0 {
			return []any{int64(0)}
		}
		return nil
	case float64:
		if v != 0 {
			return []any{float64(0)}
		}
		return nil
	case json.Number:
		if v != "0" {
			return []any{json.Number("0")}
		}
		return nil
	default:
		return nil
	}
}
