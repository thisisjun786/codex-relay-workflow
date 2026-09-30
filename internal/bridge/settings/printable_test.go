package settings

import (
	"fmt"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport/golden"
)

// The table is str.isprintable() of every code point as CPython 3.14 answers it (decision 49):
// the golden, one digit per code point with every surrogate 0, began as that interpreter's
// answer, and the table is frozen, so a difference means the table changed.
func TestPrintableIsCPython314sIsprintable(t *testing.T) {
	table := make([]byte, 0x110000)
	for c := range rune(0x110000) {
		table[c] = '0'
		if Printable(c) {
			table[c] = '1'
		}
	}
	golden.Check(t, "unicode version", []byte(printableVersion))
	golden.Check(t, "isprintable", table, golden.Compare(func(want, got []byte) error {
		if len(want) != len(got) {
			return fmt.Errorf("%d code points, the golden %d", len(got), len(want))
		}
		differ, first := 0, rune(-1)
		for c := range want {
			if want[c] != got[c] {
				if differ++; first < 0 {
					first = rune(c)
				}
			}
		}
		if differ > 0 {
			return fmt.Errorf("%d code points differ, the first U+%04X (Printable %c, the golden %c)", differ, first, got[first], want[first])
		}
		return nil
	}))
}
