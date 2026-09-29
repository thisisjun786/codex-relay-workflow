package settings

import "testing"

// u is the character r; esc is Python's \u escape of the BMP code point hex.
func u(r rune) string       { return string(r) }
func esc(hex string) string { return `\u` + hex }

// Each want is CPython 3.14's repr() of the same str, the input spelled as a Go string holds it:
// a lone surrogate as the WTF-8 bytes a JSON decoder keeps, or a byte that is not UTF-8 as a path
// or argv holds it where Python holds the surrogateescape code point.
func TestRepr_escapes_what_python_does_not_print(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{"a" + u(0xa0) + "b", `'a\xa0b'`},                                                  // Zs other than the space
		{u(0xe9) + u(0x200b) + u(0x1f600), "'" + u(0xe9) + esc("200b") + u(0x1f600) + "'"}, // Cf between printables
		{"x" + u(0x180e), "'x" + esc("180e") + "'"},                                        // Cf
		{"/n" + u(0x85) + "x", `'/n\x85x'`},                                                // Cc above 0x7f
		{"l" + u(0x2028) + "p", "'l" + esc("2028") + "p'"},                                 // Zl
		{u(0xad), `'\xad'`},                                                                // Cf below 0x100
		{u(0x3000), "'" + esc("3000") + "'"},                                               // Zs
		{u(0xe000), "'" + esc("e000") + "'"},                                               // Co
		{u(0x378), "'" + esc("0378") + "'"},                                                // Cn
		{u(0xe0001), `'\U000e0001'`},                                                       // Cf above the BMP
		{"\xed\xa0\x80", "'" + esc("d800") + "'"},                                          // a lone surrogate kept as WTF-8
		{"p\xed\xb2\x80.json", "'p" + esc("dc80") + ".json'"},                              // the surrogateescape range, as WTF-8
		{"/r\x80\xff", "'/r" + esc("dc80") + esc("dcff") + "'"},                            // bytes that are not UTF-8
		{u(0xfffd), "'" + u(0xfffd) + "'"},                                                 // the replacement character prints
		{"tab\t\x7f\x00", `'tab\t\x7f\x00'`},
		{`\`, `'\\'`},
		{"it's", `"it's"`},
		{`it's "q"`, `'it\'s "q"'`},
		{" ", "' '"},
	} {
		if got := Repr(c.in); got != c.want {
			t.Errorf("Repr(%q) = %s, want %s", c.in, got, c.want)
		}
	}
}
