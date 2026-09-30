package pyjson

import (
	"encoding/hex"
	"strings"
	"testing"
)

// Each want is what CPython 3.13 answered for bytes.fromhex(hex).decode("utf-8", "replace"):
// one U+FFFD for every sequence the strict decoder refuses (an encoded surrogate refused at its
// first byte, so three), decoding carrying on after it.
func TestDecodeReplace_is_bytes_decode_replace(t *testing.T) {
	for _, c := range []struct{ hex, want string }{
		{"706c61696e", "plain"},
		{"636166c3a9", "caf\u00e9"},
		{"61ff62", "a\ufffdb"},
		{"e28278", "\ufffdx"},
		{"eda080", "\ufffd\ufffd\ufffd"},
		{"c0af", "\ufffd\ufffd"},
		{"f4908080", "\ufffd\ufffd\ufffd\ufffd"},
		{"f09f", "\ufffd"},
		{"e08080", "\ufffd\ufffd\ufffd"},
		{"efbfbd", "\ufffd"},
		{"8080", "\ufffd\ufffd"},
		{"f888", "\ufffd\ufffd"},
		{"78e282", "x\ufffd"},
		{"f09f9880f09f98", "\U0001F600\ufffd"},
	} {
		raw, err := hex.DecodeString(c.hex)
		if err != nil {
			t.Fatal(err)
		}
		if got := DecodeReplace(raw); got != c.want {
			t.Errorf("DecodeReplace(%s) = %+q, python %+q", c.hex, got, c.want)
		}
	}
}

// Each want is the text json.JSONDecoder().raw_decode(doc) read in CPython 3.13, s[:end], or ""
// where it raised ValueError.
func TestRawDecodePrefix_is_raw_decode(t *testing.T) {
	digits := strings.Repeat("7", 4300)
	for _, c := range []struct{ doc, want string }{
		{`{"a": [NaN, -Infinity, Infinity]} junk`, `{"a": [NaN, -Infinity, Infinity]}`},
		{`[1, 2]]`, `[1, 2]`},
		{`"x" "y"`, `"x"`},
		{` {"a": 1}`, ""},
		{`{"a": Nan}`, ""},
		{`[1e400, -0.0]x`, `[1e400, -0.0]`},
		{`{"a": 1`, ""},
		{"[" + digits + "7]", ""},
		{"[" + digits + "]tail", "[" + digits + "]"},
		{`NaNx`, `NaN`},
		{`-Infinityy`, `-Infinity`},
		{`{"a": "\ud800"}!`, `{"a": "\ud800"}`},
		{"{\"\u00e9\": \"\ufffd\"} \ufffd", "{\"\u00e9\": \"\ufffd\"}"},
		{"", ""},
	} {
		got, ok := RawDecodePrefix(c.doc)
		if ok != (c.want != "") || got != c.want {
			t.Errorf("RawDecodePrefix(%.40q) = %.40q, %v; python %.40q", c.doc, got, ok, c.want)
		}
	}
}
