//go:build dev

package cxcfuzz

import (
	"encoding/json"
	"math/rand"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson"
)

// canonical is pyjson.Dumps with sorted keys and surrogate escapes, whatever it does to be quick on a long
// string. The recursion-limit answer of the pyjson oracle holds a two-megabyte traceback, and a campaign wrote it
// out four times per case (CRW-1066).
func canonicalReference(value any) string {
	return pyjson.Dumps(value, pyjson.Options{SortKeys: true, Bytes: pyjson.SurrogateEscapes})
}

// asciiBytes are the 128 ASCII characters in a fixed order, the controls, the quote, the backslash and DEL included.
func asciiBytes() string {
	var b strings.Builder
	for c := 0; c < 128; c++ {
		b.WriteByte(byte(c))
	}
	return b.String()
}

// randomText is a string of n bytes drawn from the alphabet; a non-ASCII alphabet makes the text one the fast path
// must leave to the writer.
func randomText(rng *rand.Rand, alphabet []rune, n int) string {
	var b strings.Builder
	for i := 0; i < n; i++ {
		b.WriteRune(alphabet[rng.Intn(len(alphabet))])
	}
	return b.String()
}

// randomValue is a decoded-answer-shaped value: objects (both as pyjson.Object and as a map), arrays, numbers,
// booleans, null and strings of every length around the fast path's threshold.
func randomValue(rng *rand.Rand, depth int, alphabets [][]rune) any {
	switch n := rng.Intn(9); {
	case n < 3 || depth <= 0:
		alphabet := alphabets[rng.Intn(len(alphabets))]
		length := []int{0, 1, 7, 100, 4000, 4096, 4097, 9000, 30000}[rng.Intn(9)]
		return randomText(rng, alphabet, length)
	case n == 3:
		return json.Number([]string{"0", "-1", "12345678901234567890", "1.5", "1e400"}[rng.Intn(5)])
	case n == 4:
		return rng.Intn(2) == 0
	case n == 5:
		return nil
	case n == 6:
		items := make([]any, rng.Intn(4))
		for i := range items {
			items[i] = randomValue(rng, depth-1, alphabets)
		}
		return items
	case n == 7:
		object := pyjson.Object{}
		for i := 0; i < rng.Intn(5); i++ {
			object = append(object, pyjson.Field{Key: []string{"b", "a", "stderr", "é", "z\"q", "\x00crwbig0\x00"}[rng.Intn(6)], Value: randomValue(rng, depth-1, alphabets)})
		}
		return object
	default:
		object := map[string]any{}
		for i := 0; i < rng.Intn(5); i++ {
			object[[]string{"b", "a", "stderr", "é", "m"}[rng.Intn(5)]] = randomValue(rng, depth-1, alphabets)
		}
		return object
	}
}

// The quick path for long strings writes exactly what the writer writes, for every ASCII character, for long texts
// with and without characters past ASCII, for lone surrogates held as WTF-8, and for strings that look like the
// quick path's own placeholders.
func TestCanonicalWritesWhatThePyjsonWriterWrites(t *testing.T) {
	ascii := []rune(asciiBytes())
	printable := []rune("abc xyz\n\t\"\\0123")
	mixed := append([]rune("aé☃😀\n"), ascii[:40]...)
	alphabets := [][]rune{ascii, printable, mixed, []rune("a")}
	rng := rand.New(rand.NewSource(1066))
	for i := 0; i < 600; i++ {
		value := randomValue(rng, 4, alphabets)
		if got, want := canonical(value), canonicalReference(value); got != want {
			t.Fatalf("value %d: canonical differs from the writer\n got  %.300q\n want %.300q", i, got, want)
		}
	}
	long := strings.Repeat(asciiBytes(), 64)
	for _, value := range []any{
		long,
		pyjson.Object{{Key: "stderr", Value: long}, {Key: "exit", Value: json.Number("1")}, {Key: "stdout", Value: ""}},
		[]any{long, long, "short", map[string]any{"k": long}},
		pyjson.Object{{Key: "text", Value: strings.Repeat("a", 5000) + "\xed\xa0\x80" + strings.Repeat("b", 5000)}},
		pyjson.Object{{Key: "text", Value: strings.Repeat("\xff", 6000)}},
		pyjson.Object{{Key: "text", Value: strings.Repeat("é", 6000)}},
		pyjson.Object{{Key: "text", Value: long + "\x00crwbig0\x00"}, {Key: "other", Value: long}},
		pyjson.Object{{Key: "\x00crwbig0\x00", Value: long}},
		nil,
		"",
		json.Number("7"),
	} {
		if got, want := canonical(value), canonicalReference(value); got != want {
			t.Fatalf("canonical differs from the writer for %.80q\n got  %.300q\n want %.300q", canonicalReference(value), got, want)
		}
	}
}

// CRW-1066's byte path reserves the long string's output instead of growing a buffer as the character writer
// does. Keep that resource bound without comparing wall times from different scheduling and GC conditions.
// Both inputs exceed the fast-path threshold and have the same escape density. canonicalReference is the
// pre-CRW-1066 implementation: the negative control must violate the same bound, not just produce valid JSON.
func TestCanonicalLongStringAllocationsStayBounded(t *testing.T) {
	for _, tc := range []struct {
		name       string
		write      func(any) string
		wantGrowth bool
	}{
		{"canonical", canonical, false},
		{"character_writer_control", canonicalReference, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var allocs [2]float64
			for i, repeats := range []int{128, 20000} {
				traceback := strings.Repeat("  File \"/usr/lib/python3.14/json/encoder.py\", line 341, in _iterencode_list\n    yield from chunks\nwhen serializing list item 0\n", repeats)
				value := pyjson.Object{{Key: "exit", Value: json.Number("1")}, {Key: "stderr", Value: traceback}, {Key: "stdout", Value: ""}}
				want := canonicalReference(value)
				var got string
				// AllocsPerRun warms the writer and uses one P; input construction and the reference output
				// are outside the measurement. Retain and check the result so an empty/no-op writer fails.
				allocs[i] = testing.AllocsPerRun(5, func() { got = tc.write(value) })
				if got != want {
					t.Fatalf("canonical differs from the writer for %d input bytes", len(traceback))
				}
				t.Logf("%d input bytes: %.0f allocations/write", len(traceback), allocs[i])
			}
			if grows := allocs[1] > allocs[0]; grows != tc.wantGrowth {
				t.Fatalf("allocations/write grew from %.0f to %.0f: want growth=%t", allocs[0], allocs[1], tc.wantGrowth)
			}
		})
	}
}
