package jsnode

import (
	"encoding/json"
	"testing"
)

func TestStringifyIsJSONStringify(t *testing.T) {
	type row struct {
		B string `json:"b"`
		A []int  `json:"a"`
	}
	for _, c := range []struct {
		name   string
		v      any
		indent string
		want   string
	}{
		{"null", nil, "", `null`},
		{"compact object keeps field order", row{"x", []int{1, 2}}, "", `{"b":"x","a":[1,2]}`},
		{"indented", row{"x", []int{1}}, "  ", "{\n  \"b\": \"x\",\n  \"a\": [\n    1\n  ]\n}"},
		{"empty containers", map[string]any{"a": []any{}, "b": map[string]any{}}, "  ", "{\n  \"a\": [],\n  \"b\": {}\n}"},
		{"html is not escaped", "<a href=\"x\">&</a>", "", `"<a href=\"x\">&</a>"`},
		{"u2028 and u2029 are literal", "a\u2028b\u2029c", "", "\"a\u2028b\u2029c\""},
		{"an escaped backslash before u2028 stays text", `\u2028`, "", `"\\u2028"`},
		{"control characters", "\x00\x1f\n\t", "", `"\u0000\u001f\n\t"`},
		{"numbers", []float64{1, 1.5, 1e21, 1e-7, 123456789}, "", `[1,1.5,1e+21,1e-7,123456789]`},
		{"raw message", json.RawMessage(`{"k":[1,2]}`), "", `{"k":[1,2]}`},
	} {
		t.Run(c.name, func(t *testing.T) {
			got, err := Stringify(c.v, c.indent)
			if err != nil || string(got) != c.want {
				t.Errorf("Stringify = %q, %v; want %q", got, err, c.want)
			}
		})
	}
	if _, err := Stringify(make(chan int), ""); err == nil {
		t.Error("an unsupported value is an error")
	}
}

func TestJSStringIsJSONStringifyOfAString(t *testing.T) {
	for _, c := range []struct{ name, in, want string }{
		{"empty", "", `""`},
		{"plain", "abc", `"abc"`},
		{"quote and backslash", `a"b\c`, `"a\"b\\c"`},
		{"named controls", "\b\f\n\r\t", `"\b\f\n\r\t"`},
		{"other controls are lowercase hex", "\x00\x01\x1f", `"\u0000\u0001\u001f"`},
		{"del is not escaped", "\x7f", "\"\x7f\""},
		{"markup stays", "<&>'", `"<&>'"`},
		{"u2028 and u2029 stay", "\u2028\u2029", "\"\u2028\u2029\""},
		{"non-ascii stays", "\u00e9\U0001f600\ud55c", "\"\u00e9\U0001f600\ud55c\""},
		{"lone high surrogate", "\xed\xa0\x80", `"\ud800"`},
		{"lone low surrogate", "\xed\xbf\xbf", `"\udfff"`},
		{"surrogate between text", "a\xed\xb0\x80b", `"a\udc00b"`},
		{"a byte outside utf-8 reads as U+FFFD", "a\xffb", "\"a\ufffdb\""},
		{"a truncated surrogate is two replacement characters", "\xed\xa0", "\"\ufffd\ufffd\""},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := JSString(c.in); got != c.want {
				t.Errorf("JSString(%q) = %s, want %s", c.in, got, c.want)
			}
		})
	}
}

// Whatever JSString writes for well-formed text reads back to that text.
func TestJSStringRoundTrips(t *testing.T) {
	for _, in := range []string{"", "plain", "a\"b\\c\n", "\x00\x1f", "\u2028\u2029", "<&>", "\u00e9\U0001f600", "\ufffd"} {
		var back string
		if err := json.Unmarshal([]byte(JSString(in)), &back); err != nil || back != in {
			t.Errorf("JSString(%q) = %s reads back as %q, %v", in, JSString(in), back, err)
		}
	}
}
