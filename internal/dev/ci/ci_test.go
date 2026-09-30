//go:build dev

package ci

import (
	"strings"
	"testing"
)

// An unknown check is echoed as repr() of the str Python holds for the argument: a character
// CPython 3.14's str.isprintable() refuses is escaped even where Go's newer Unicode tables
// print it (U+0C5C is assigned in Unicode 17), and an argv byte that is not UTF-8 is the
// surrogate Python decodes it to. Each expectation is CPython 3.14.4's repr.
func TestAnUnknownCheckIsEchoedAsPythonsRepr(t *testing.T) {
	for _, c := range []struct{ arg, want string }{
		{"x\U00000c5c", `'x\u0c5c'`},
		{"x\xff", `'x\udcff'`},
		{"x\U000000a0", `'x\xa0'`},
		{"it's", `"it's"`},
	} {
		got := runCommand(t, repoRoot(), nil, crwDev, "ci", c.arg)
		line := "crw-dev ci: error: invalid choice: " + c.want + " (choose from contracts, gate, operations, plugin, scope, validate)\n"
		if got.code != 2 || !strings.HasSuffix(got.stderr, line) {
			t.Errorf("crw-dev ci %q: exit %d\n%s\nwant the line %s", c.arg, got.code, got.stderr, line)
		}
	}
}
