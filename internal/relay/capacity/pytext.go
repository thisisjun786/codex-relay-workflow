package capacity

import (
	"fmt"
	"math"
	"strconv"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// Python spellings this package needs for caller-visible text: repr() of a str and a float,
// str() of a float, and json.dumps() with its default separators for journal details.

func repr(text string) string { return store.PythonRepr(text) }

// pyFloat is float.__repr__ (and str()): fixed notation for exponents in [-4, 16).
func pyFloat(v float64) string {
	switch {
	case math.IsNaN(v):
		return "nan"
	case math.IsInf(v, 1):
		return "inf"
	case math.IsInf(v, -1):
		return "-inf"
	case v == 0:
		if math.Signbit(v) {
			return "-0.0"
		}
		return "0.0"
	}
	shortest := strconv.FormatFloat(v, 'e', -1, 64)
	_, exp, _ := strings.Cut(shortest, "e")
	exponent, _ := strconv.Atoi(exp)
	if exponent >= -4 && exponent < 16 {
		text := strconv.FormatFloat(v, 'f', -1, 64)
		if !strings.Contains(text, ".") {
			text += ".0"
		}
		return text
	}
	mantissa, _, _ := strings.Cut(shortest, "e")
	sign := "+"
	if exponent < 0 {
		sign, exponent = "-", -exponent
	}
	return fmt.Sprintf("%se%s%02d", mantissa, sign, exponent)
}
