package jsnode

import (
	"math"
	"strconv"
	"strings"
)

// NumberString is Number.prototype.toString for radix 10 (String(n), a number in a template literal): the shortest round-trip decimal,
// fixed notation for 1e-6 <= |n| < 1e21 and exponential outside, "NaN", "Infinity" and "-Infinity" for the non-finite values, and "0"
// for a zero of either sign. Go's %v would print 1e+06 where JS prints 1000000.
func NumberString(f float64) string {
	switch {
	case math.IsNaN(f):
		return "NaN"
	case math.IsInf(f, 1):
		return "Infinity"
	case math.IsInf(f, -1):
		return "-Infinity"
	case f == 0:
		return "0"
	}
	negative := math.Signbit(f)
	if negative {
		f = -f
	}
	// The shortest round-trip mantissa and the exponent of its leading digit.
	mantissa := strconv.FormatFloat(f, 'e', -1, 64)
	at := strings.IndexByte(mantissa, 'e')
	exponent, _ := strconv.Atoi(mantissa[at+1:])
	digits := strings.Replace(mantissa[:at], ".", "", 1)
	digits = strings.TrimRight(digits, "0")
	if digits == "" {
		digits = "0"
	}
	k := len(digits)
	n := exponent + 1 // the value is 0.digits * 10^n
	var out string
	switch {
	case k <= n && n <= 21:
		out = digits + strings.Repeat("0", n-k)
	case 0 < n && n <= 21:
		out = digits[:n] + "." + digits[n:]
	case -6 < n && n <= 0:
		out = "0." + strings.Repeat("0", -n) + digits
	default:
		out = digits[:1]
		if k > 1 {
			out += "." + digits[1:]
		}
		ex := n - 1
		sign := "+"
		if ex < 0 {
			sign = "-"
			ex = -ex
		}
		out += "e" + sign + strconv.Itoa(ex)
	}
	if negative {
		return "-" + out
	}
	return out
}
