package quote

import "testing"

// Shell is shlex.quote: the safe set stands for itself, ” is the empty word, any other text is
// single-quoted and an embedded quote is closed, written and reopened.
func TestShellQuotesAsShlexQuoteDoes(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{"", "''"},
		{"codex-session-relay", "codex-session-relay"},
		{"/var/lib/relay/state.d", "/var/lib/relay/state.d"},
		{"--state=/x/y", "--state=/x/y"},
		{"a@b%c+d=e:f,g./-_h", "a@b%c+d=e:f,g./-_h"},
		{"0123456789AZaz", "0123456789AZaz"},
		{"two words", "'two words'"},
		{"it's", `'it'"'"'s'`},
		{"'", `''"'"''`},
		{"a$b", "'a$b'"},
		{"a`b`", "'a`b`'"},
		{"semi;colon", "'semi;colon'"},
		{"star*", "'star*'"},
		{"tilde~", "'tilde~'"},
		{"new\nline", "'new\nline'"},
		{"é", "'é'"},
		{"a\x00b", "'a\x00b'"},
		{"bad\xffbyte", "'bad\xffbyte'"},
	} {
		if got := Shell(c.in); got != c.want {
			t.Errorf("Shell(%q) = %s, want %s", c.in, got, c.want)
		}
	}
}
