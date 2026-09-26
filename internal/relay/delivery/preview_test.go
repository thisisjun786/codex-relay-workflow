package delivery

import (
	"encoding/json"
	"testing"
)

// Values from the real report.inline / unheaded / known on this checkout's Python.
func TestText_helpers_match_python_report(t *testing.T) {
	for input, want := range map[string]string{"x": "x", "a\nb": "a / b", "a\r\nb\n": "a / b", "\n": "", "": "",
		"a\x1cb": "a / b", "  \nz": "z", "a\u2028b": "a / b"} {
		if got := inline(input); got != want {
			t.Errorf("inline(%q) = %q, want %q", input, got, want)
		}
	}
	for input, want := range map[string]string{"FIX SCOPE": `"FIX SCOPE"`, "- **fix scope**: x": "- **fix scope**: x",
		"SCOPE:": `"SCOPE:"`, "scoped": "scoped", "> VERDICT": `"> VERDICT"`, "`MUST DO`: y": "`MUST DO`: y"} {
		if got := unheaded(input); got != want {
			t.Errorf("unheaded(%q) = %q, want %q", input, got, want)
		}
	}
	for _, c := range []struct {
		value any
		want  string
	}{{nil, "not recorded"}, {"", "not recorded"}, {json.Number("0"), "0"}, {1.0, "1.0"}, {json.Number("2"), "2"},
		{false, "False"}, {[]any{json.Number("1"), "a"}, "[1, 'a']"}} {
		if got := known(c.value); got != c.want {
			t.Errorf("known(%v) = %q, want %q", c.value, got, c.want)
		}
	}
}
