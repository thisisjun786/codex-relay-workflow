package delivery

import "testing"

// int(str) refuses a literal naming it as repr() does, so a character CPython 3.14's
// str.isprintable() refuses is escaped even where Go's newer Unicode tables print it (U+0C5C is
// assigned in Unicode 17). Each expectation is CPython 3.14.4's ValueError text.
func TestAnIntegerLiteralIsRefusedNamingItAsPythonsRepr(t *testing.T) {
	for _, c := range []struct{ literal, want string }{
		{"\U00000c5c1", `ValueError: invalid literal for int() with base 10: '\u0c5c1'`},
		{"x\U000000a0", `ValueError: invalid literal for int() with base 10: 'x\xa0'`},
		{"1\U0000200b", `ValueError: invalid literal for int() with base 10: '1\u200b'`},
		{"it's", `ValueError: invalid literal for int() with base 10: "it's"`},
		{"\U00000663x", "ValueError: invalid literal for int() with base 10: '\U00000663x'"},
	} {
		value, err := sqliteIntString(c.literal)
		if err == nil || err.Error() != c.want {
			t.Errorf("int(%q) = %v, %v; want %s", c.literal, value, err, c.want)
		}
	}
}
