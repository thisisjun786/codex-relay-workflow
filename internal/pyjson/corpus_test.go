package pyjson_test

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"math/big"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson"
	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson/pyjsontest"
	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport/golden"
)

// The readings and writings the product uses, each named for the reader or writer it replaced,
// over the corpus. What each gives the generated documents is a golden in full; what it gives the
// whole sample is a golden digest. Each was proven byte for byte against the copy it replaced
// (the pyjson_fold_test.go of the fold's first step) before that copy was deleted.
var readings = map[string]pyjson.LoadOptions{
	"store.decodeOrdered":       {Numbers: pyjson.SpelledNumbers},
	"store.LoadsJSON":           {Python: true, Constants: true},
	"store.decodePythonJSON":    {Constants: true, Surrogates: true, Numbers: pyjson.BigNumbers, Deep: true},
	"registry.decodeJSON":       {},
	"registry.DecodeJSON":       {Python: true},
	"delivery.loads":            {Numbers: pyjson.Int64FloatNumbers, RangeErrors: true, Trailing: pyjson.TrailingClose},
	"faults.loads":              {Map: true, Numbers: pyjson.SpelledNumbers, Trailing: pyjson.TrailingAnything},
	"managed.decodeOrderedJSON": {Numbers: pyjson.SpelledNumbers, Repeats: true, Trailing: pyjson.TrailingAnything},
}

var writings = map[string]pyjson.Options{
	"store.receiptRecord":       {Normalize: true},
	"store.quoteJSON":           {},
	"registry.pyDumps":          {},
	"registry.canonical":        {SortKeys: true},
	"delivery.canonical":        {Compact: true, SortKeys: true},
	"delivery.jsonCompact":      {Compact: true, SortKeys: true, Unicode: true, Bytes: pyjson.ReplacedAll},
	"faults.dumps":              {SortKeys: true, Unicode: true, Bytes: pyjson.ReplacedAll},
	"faults.dumps.compact":      {Compact: true, SortKeys: true, Unicode: true, Bytes: pyjson.ReplacedAll},
	"managed.compactPythonJSON": {Compact: true, SortKeys: true, Marshal: true, UnescapeHTML: true},
	"managed.settingsJSON":      {SortKeys: true, Marshal: true},
}

func TestReadings_are_the_goldens(t *testing.T) {
	sample := pyjsontest.Sample(t)
	for _, name := range sortedKeys(readings) {
		options := readings[name]
		render := func(doc string) string {
			value, err := pyjson.Loads(doc, options)
			if err != nil {
				return fmt.Sprintf("%q refused: %s", doc, err)
			}
			return fmt.Sprintf("%q read: %s", doc, typed(value))
		}
		golden.Check(t, name+"/edge", []byte(renderAll(pyjsontest.Edge(), render)))
		golden.Check(t, name+"/sample", []byte(digest(sample, render)))
	}
}

func TestWritings_are_the_goldens(t *testing.T) {
	read := func(docs []string) []any {
		var values []any
		for _, options := range []pyjson.LoadOptions{
			{Python: true, Constants: true, Surrogates: true, Numbers: pyjson.BigNumbers},
			{Numbers: pyjson.SpelledNumbers, Map: true},
		} {
			for _, doc := range docs {
				// An indent grows with the depth: a value 10000 deep is written a line per level.
				if len(doc) > 1<<14 {
					continue
				}
				if value, err := pyjson.Loads(doc, options); err == nil {
					values = append(values, value)
				}
			}
		}
		return values
	}
	edge := append(read(pyjsontest.Edge()), pyjsontest.Values()...)
	sample := read(pyjsontest.Sample(t))
	for _, name := range sortedKeys(writings) {
		options := writings[name]
		render := func(value any) string {
			data, err := pyjson.Encode(value, options)
			if err != nil {
				return fmt.Sprintf("%s refused: %s", typed(value), err)
			}
			return fmt.Sprintf("%s written: %s", typed(value), data)
		}
		golden.Check(t, name+"/edge", []byte(renderAll(edge, render)))
		golden.Check(t, name+"/sample", []byte(digest(sample, render)))
	}
}

func TestFloat_is_the_golden(t *testing.T) {
	render := func(f float64) string { return strconv.FormatFloat(f, 'g', -1, 64) + " " + pyjson.Float(f) }
	golden.Check(t, "Float", []byte(renderAll(pyjsontest.Floats(), render)))
}

func renderAll[T any](items []T, render func(T) string) string {
	var b strings.Builder
	for _, item := range items {
		b.WriteString(render(item))
		b.WriteByte('\n')
	}
	return b.String()
}

func digest[T any](items []T, render func(T) string) string {
	hash := sha256.New()
	for _, item := range items {
		hash.Write([]byte(render(item)))
		hash.Write([]byte{'\n'})
	}
	return fmt.Sprintf("%d items, sha256 %s\n", len(items), hex.EncodeToString(hash.Sum(nil)))
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	return keys
}

// typed spells a value with its Go types: which of json.Number, int64, *big.Int and float64 a
// number is, a string's bytes as Go quotes them, and an Object's fields in order.
func typed(value any) string {
	switch v := value.(type) {
	case nil:
		return "null"
	case bool:
		return strconv.FormatBool(v)
	case string:
		return strconv.Quote(v)
	case json.Number:
		return "number(" + string(v) + ")"
	case int:
		return "int(" + strconv.Itoa(v) + ")"
	case int64:
		return "int64(" + strconv.FormatInt(v, 10) + ")"
	case *big.Int:
		return "big(" + v.String() + ")"
	case float64:
		if v == 0 && math.Signbit(v) {
			return "float(-0)"
		}
		return "float(" + strconv.FormatFloat(v, 'g', -1, 64) + ")"
	case pyjson.Object:
		parts := make([]string, len(v))
		for i, field := range v {
			parts[i] = strconv.Quote(field.Key) + ": " + typed(field.Value)
		}
		return "{" + strings.Join(parts, ", ") + "}"
	case map[string]any:
		keys := sortedKeys(v)
		parts := make([]string, len(keys))
		for i, key := range keys {
			parts[i] = strconv.Quote(key) + ": " + typed(v[key])
		}
		return "map{" + strings.Join(parts, ", ") + "}"
	case []any:
		parts := make([]string, len(v))
		for i, item := range v {
			parts[i] = typed(item)
		}
		return "[" + strings.Join(parts, ", ") + "]"
	}
	return fmt.Sprintf("%T(%v)", value, value)
}
