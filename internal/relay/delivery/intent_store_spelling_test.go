package delivery

import "testing"

// The store an intent names is spelled as str(Path(value)) spells it: exactly two leading slashes
// stay a root of their own (pathlib keeps them), three or more fold to one, "." parts and repeated
// or trailing slashes go and ".." stays.
func TestExpandedStoreSpellsThePathAsPathlibDoes(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{"/var/relay/relay.sqlite3", "/var/relay/relay.sqlite3"},
		{"//var/relay//relay.sqlite3", "//var/relay/relay.sqlite3"},
		{"///var/relay/./x/../relay.sqlite3/", "/var/relay/x/../relay.sqlite3"},
		{"relay.sqlite3", "relay.sqlite3"},
		{"", "."},
	} {
		got, err := expandedStore(c.in)
		if err != nil || got != c.want {
			t.Errorf("expandedStore(%q) = %q, %v; want %q", c.in, got, err, c.want)
		}
	}
}
