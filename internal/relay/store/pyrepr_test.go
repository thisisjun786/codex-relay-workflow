package store

import (
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/pyvalue"
)

// PyRepr is repr() of a str as CPython 3.14.4 prints it: its quote choice, its escapes of every
// character str.isprintable() refuses, and a lone surrogate (an argv byte that is not UTF-8, or
// the WTF-8 form a JSON decoder keeps) as \udXXX. Each expectation is that interpreter's answer.
func TestPyReprIsPythonsReprOfAStr(t *testing.T) {
	t.Parallel()
	for _, c := range []struct{ text, want string }{
		{"plain", `'plain'`},
		{"x\u00a0y", `'x\xa0y'`},
		{"x\u2028y", `'x\u2028y'`},
		{"x\u200by", `'x\u200by'`},
		{"x\xffy", `'x\udcffy'`},
		{"x\xed\xb3\xbfy", `'x\udcffy'`},
		{"it's", `"it's"`},
		{"a\"b'c", `'a"b\'c'`},
		{"x\u0c5cy", `'x\u0c5cy'`}, // assigned in Unicode 17, unassigned in CPython 3.14's 16.0.0
		{"caf\u00e9 \u3042", "'caf\u00e9 \u3042'"},
		{"t\tn\nr\r\\\x01\x7f", `'t\tn\nr\r\\\x01\x7f'`},
		{"\U0001f600\U000e0001", "'\U0001f600\\U000e0001'"},
	} {
		if got := pyvalue.StrRepr(c.text); got != c.want {
			t.Errorf("pyvalue.StrRepr(%q) = %s, want %s", c.text, got, c.want)
		}
	}
}
