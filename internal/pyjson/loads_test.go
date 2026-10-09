package pyjson_test

import (
	"encoding/json"
	"runtime/debug"
	"slices"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson"
	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson/pyjsontest"
)

// A Python reading refuses exactly what json.loads refuses, with its text; an encoding/json
// reading refuses exactly what encoding/json refuses.
func TestLoads_refuses_what_its_reading_refuses(t *testing.T) {
	for _, doc := range pyjsontest.Sample(t) {
		_, err := pyjson.Loads(doc, pyjson.LoadOptions{Python: true, Constants: true, Deep: true})
		if message := pyjson.Error(doc); (err == nil) != (message == "") || err != nil && err.Error() != message {
			t.Errorf("Loads(%q) python reading: %v, json.loads: %q", doc, err, message)
		}
		_, err = pyjson.Loads(doc, pyjson.LoadOptions{})
		if valid := json.Valid([]byte(doc)); valid != (err == nil) {
			t.Errorf("Loads(%q) encoding/json reading: %v, json.Valid %v", doc, err, valid)
		}
	}
}

// What Dumps writes of a value Loads read is read back as the same value.
func TestDumps_writes_what_Loads_reads_back(t *testing.T) {
	options := pyjson.LoadOptions{Python: true, Constants: true, Surrogates: true, Numbers: pyjson.BigNumbers}
	for _, doc := range pyjsontest.Sample(t) {
		value, err := pyjson.Loads(doc, options)
		if err != nil || len(doc) > 1<<14 {
			continue // an indent grows with the depth: a document 10000 deep is written a line per level
		}
		for _, o := range []pyjson.Options{{}, {Indent: 2, SortKeys: true}, {Compact: true, Unicode: true}} {
			text := pyjson.Dumps(value, o)
			again, err := pyjson.Loads(text, options)
			want := value
			if o.SortKeys {
				want = sortedFields(value)
			}
			if err != nil || !pyjsontest.Same(want, again) {
				t.Errorf("Dumps(Loads(%q), %+v) = %q, read back as %#v, %v", doc, o, text, again, err)
			}
		}
	}
}

// sortedFields is value with every Object's fields in the byte order of their keys, the order
// SortKeys writes them in, so a SortKeys round trip is compared value by value.
func sortedFields(value any) any {
	switch v := value.(type) {
	case pyjson.Object:
		if v == nil {
			return v
		}
		fields := make(pyjson.Object, len(v))
		for i, field := range v {
			fields[i] = pyjson.Field{Key: field.Key, Value: sortedFields(field.Value)}
		}
		slices.SortStableFunc(fields, func(x, y pyjson.Field) int { return strings.Compare(x.Key, y.Key) })
		return fields
	case []any:
		if v == nil {
			return v
		}
		items := make([]any, len(v))
		for i := range v {
			items[i] = sortedFields(v[i])
		}
		return items
	}
	return value
}

// CRW-1075. A Deep reading reads a container without a call per level, so a document read to millions of levels
// needs no goroutine stack, and it reads every document the recursive reading reads to the same value and the
// same refusal.
func TestLoads_deep_reading_agrees_with_the_recursive_one(t *testing.T) {
	readings := []pyjson.LoadOptions{
		{}, {Map: true}, {Repeats: true}, {Unique: true}, {Unique: true, Map: true},
		{Constants: true, Numbers: pyjson.SpelledNumbers}, {Surrogates: true, Numbers: pyjson.BigNumbers},
		{Trailing: pyjson.TrailingClose}, {Trailing: pyjson.TrailingAnything},
	}
	for _, doc := range pyjsontest.Sample(t) {
		for _, o := range readings {
			plain, plainErr := pyjson.Loads(doc, o)
			if plainErr != nil && strings.Contains(plainErr.Error(), "max depth") {
				continue // the recursive reading's own limit
			}
			o.Deep = true
			deep, deepErr := pyjson.Loads(doc, o)
			if (plainErr == nil) != (deepErr == nil) || plainErr != nil && plainErr.Error() != deepErr.Error() {
				t.Errorf("Loads(%.60q, %+v): recursive %v, deep %v", doc, o, plainErr, deepErr)
				continue
			}
			if plainErr == nil && !pyjsontest.Same(plain, deep) {
				t.Errorf("Loads(%.60q, %+v): recursive %#v, deep %#v", doc, o, plain, deep)
			}
		}
	}
}

func TestLoads_deep_reading_needs_no_stack_for_the_nesting_of_its_document(t *testing.T) {
	defer debug.SetMaxStack(debug.SetMaxStack(16 << 20))
	const levels = 1_500_000
	opened, closed := strings.Repeat("[", levels), strings.Repeat("]", levels)
	deep := pyjson.LoadOptions{Map: true, Numbers: pyjson.SpelledNumbers, Deep: true}
	cases := []struct {
		name, doc string
		ok        bool
	}{
		{"closed arrays", opened + closed, true},
		{"closed objects", strings.Repeat(`{"a":`, levels/3) + "1" + strings.Repeat("}", levels/3), true},
		{"unclosed arrays", opened, false},
		{"unclosed past what the closers left allow", opened + closed[:levels/2], false},
		{"closed arrays and trailing text", opened + closed + "x", false},
		{"unclosed arrays and invalid UTF-8", opened + "\xff\xff", false},
	}
	for _, c := range cases {
		if _, err := pyjson.Loads(c.doc, deep); (err == nil) != c.ok {
			t.Errorf("%s: %v, want ok=%v", c.name, err, c.ok)
		}
	}
}
