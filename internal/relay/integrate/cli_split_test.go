package integrate

import (
	"reflect"
	"testing"
)

// CRW-965 revision (d5): an escaped character always starts an argument. The escaped branch used to write the character
// without marking the argument as started, so an escaped character followed by whitespace or the end was lost.
func TestSplitCommandKeepsAnEscapedCharacterAsAnArgument(t *testing.T) {
	cases := []struct {
		name, command string
		want          []string
	}{
		{"an escaped digit before the end", "/opt/verify \\1", []string{"/opt/verify", "1"}},
		{"an escaped space as the whole argument", "/opt/verify \\ ", []string{"/opt/verify", " "}},
		{"an escaped character outside quotes inside a word", "/opt/a\\b", []string{"/opt/ab"}},
		{"an escaped double quote inside double quotes", "/opt/\"a\\\"b\"", []string{"/opt/a\"b"}},
		{"an empty quoted argument survives", "/opt/verify \"\"", []string{"/opt/verify", ""}},
		{"an escaped character followed by a word", "/opt/verify \\1 two", []string{"/opt/verify", "1", "two"}},
	}
	for _, c := range cases {
		got, err := splitCommand(c.command)
		if err != nil || !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: splitCommand(%q) = %q (err %v); want %q", c.name, c.command, got, err, c.want)
		}
	}
}

// A trailing escape has nothing to escape and is refused, as before.
func TestSplitCommandRefusesATrailingEscape(t *testing.T) {
	if _, err := splitCommand("/opt/verify \\"); err == nil {
		t.Fatal("a trailing escape should be refused")
	}
}
