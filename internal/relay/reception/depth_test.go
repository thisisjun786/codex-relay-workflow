package reception

import (
	"strconv"
	"strings"
	"testing"
)

// TestJSONDepthIsBounded: a JSON document the relay reads (a packet, a reception ledger, a
// launch-policy declaration) is refused past 9998 nested containers and recorded settings past
// 9997, naming the bound; anything shallower is read.
func TestJSONDepthIsBounded(t *testing.T) {
	nested := func(depth int) []byte {
		return []byte(strings.Repeat("[", depth) + "0" + strings.Repeat("]", depth))
	}
	for _, c := range []struct {
		name    string
		problem func([]byte) string
		bound   int
	}{{"reader", JSONReaderDepthProblem, 9998}, {"settings", JSONSettingsDepthProblem, 9997}} {
		if got := c.problem(nested(c.bound)); got != "" {
			t.Errorf("%s: %d levels refused: %q", c.name, c.bound, got)
		}
		if got := c.problem(nested(c.bound + 1)); !strings.Contains(got, strconv.Itoa(c.bound)) {
			t.Errorf("%s: %d levels: %q does not name the bound", c.name, c.bound+1, got)
		}
		if got := c.problem([]byte(`["` + strings.Repeat("[", c.bound+1) + `"]`)); got != "" {
			t.Errorf("%s: brackets inside a string counted: %q", c.name, got)
		}
	}
}
