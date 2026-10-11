package jsnode

import (
	"testing"
	"unicode/utf8"
)

// Expected values are what Node 24 prints for Buffer.from(bytes).toString("utf8"): one U+FFFD for each maximal invalid subpart, which the
// WHATWG decoder defines (the table is lifted from the tests of the copies this package replaced: harness, relay/job, host, spawn).
func TestDecodeUTF8IsBufferToString(t *testing.T) {
	const x = "\ufffd"
	for _, c := range []struct{ in, want string }{
		{"", ""},
		{"plain", "plain"},
		{"\xc3\xa9", "\u00e9"},
		{"\xe2\x82\xac", "\u20ac"},
		{"\xf0\x9f\x98\x80", "\U0001f600"},
		{"\xe2\x82\x41", x + "A"},
		{"\xe2\x82", x},
		{"\xff\xfe", x + x},
		{"\xed\xa0\x80", x + x + x},
		{"\xed\xbf\xbf", x + x + x},
		{"\xf0\x80\x80\x80", x + x + x + x},
		{"\xf4\x90\x80\x80", x + x + x + x},
		{"\xc0\x80", x + x},
		{"\xc1\xbf", x + x},
		{"\xe0\x80\x80", x + x + x},
		{"\xe0\xa0", x},
		{"\xf0\x9f\x98", x},
		{"\xf0\x90\x80", x},
		{"\xf4\x8f\xbf", x},
		{"\xf5\x80\x80\x80", x + x + x + x},
		{"\x61\xe2\x82\xac\xe2", "a\u20ac" + x},
		{"\xf0\x9f\x98\x80\x80", "\U0001f600" + x},
		{"\xa9z", x + "z"},
		{"a\xe2\x80", "a" + x},
		{"\x80\x80", x + x},
		{"\xef\xbf\xbd", x},
		{"\xef\xbb\xbf{}", "\ufeff{}"},
		{"a\xffb\xe2\x82c\xc0\x80\xf0\x9f\x98A\xed\xa0\x80", "a" + x + "b" + x + "c" + x + x + x + "A" + x + x + x},
		{"\xe1\x80\xe1\x80\x80", x + "\u1000"},
	} {
		got := DecodeUTF8([]byte(c.in))
		if got != c.want {
			t.Errorf("DecodeUTF8(%q) = %q, want %q", c.in, got, c.want)
		}
		if !utf8.ValidString(got) {
			t.Errorf("DecodeUTF8(%q) = %q is not valid UTF-8", c.in, got)
		}
	}
}

// The decoder never holds on to its argument and leaves valid input byte for byte.
func TestDecodeUTF8ValidInputIsKept(t *testing.T) {
	in := []byte("caf\u00e9 \U0001f600 \u2028 \x00")
	got := DecodeUTF8(in)
	in[0] = 'X'
	if got != "caf\u00e9 \U0001f600 \u2028 \x00" {
		t.Errorf("DecodeUTF8 changed or aliased valid input: %q", got)
	}
}
