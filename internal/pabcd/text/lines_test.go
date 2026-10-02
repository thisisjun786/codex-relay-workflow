package text

import (
	"slices"
	"testing"
)

func TestSplitLines(t *testing.T) {
	for _, c := range []struct {
		in        string
		lines     []string
		byteExact []string
	}{
		{"a\r\nb\nc", []string{"a", "b", "c"}, []string{"a\r", "b", "c"}},
		{"a\r\nb\r\nc", []string{"a", "b", "c"}, []string{"a\r", "b\r", "c"}},
		{"a\nb\nc", []string{"a", "b", "c"}, []string{"a", "b", "c"}},
		{"", []string{""}, []string{""}},
		{"a\rb", []string{"a\rb"}, []string{"a\rb"}}, // a lone CR is content, not a line ending
	} {
		if got := SplitLines(c.in); !slices.Equal(got, c.lines) {
			t.Errorf("SplitLines(%q) = %q, want %q", c.in, got, c.lines)
		}
		if got := SplitLinesByteExact(c.in); !slices.Equal(got, c.byteExact) {
			t.Errorf("SplitLinesByteExact(%q) = %q, want %q", c.in, got, c.byteExact) // the CR keeps recorded offsets exact
		}
	}
}

func TestDominantEOLPicksCRLFOnlyWhenItLeads(t *testing.T) {
	for in, want := range map[string]EOL{"a\r\nb\r\nc\nd": CRLF, "a\nb\r\n": LF, "": LF, "no newlines at all": LF} {
		if got := DominantEOL(in); got != want {
			t.Errorf("DominantEOL(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestWithEOL(t *testing.T) {
	text := "alpha\r\nbeta\ngamma\r\n"
	for _, c := range []struct {
		in   string
		eol  EOL
		want string
	}{
		{text, LF, "alpha\nbeta\ngamma\n"},
		{"alpha\r\nbeta\r\n", CRLF, "alpha\r\nbeta\r\n"}, // a CR is never doubled
		{"a\nb", CRLF, "a\r\nb"},                         // and whether the text ended with a newline is kept
		{"a\nb\n", CRLF, "a\r\nb\r\n"},
	} {
		if got := WithEOL(c.in, c.eol); got != c.want {
			t.Errorf("WithEOL(%q, %q) = %q, want %q", c.in, c.eol, got, c.want)
		}
	}
	if WithEOL(WithEOL(text, CRLF), LF) != WithEOL(text, LF) {
		t.Error("round trip")
	}
}

func TestTrimIsJavaScriptTrim(t *testing.T) {
	for in, want := range map[string]string{"  OFF\t\n": "OFF", "\ufeff\u00a0on\u3000": "on", "\u0085x\u0085": "\u0085x\u0085", "": ""} {
		if got := Trim(in); got != want {
			t.Errorf("Trim(%q) = %q, want %q", in, got, want)
		}
	}
}
