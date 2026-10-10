package jsnode

import (
	"math"
	"strconv"
	"testing"
)

// Number.prototype.toString: fixed notation for 1e-6 <= |n| < 1e21, exponential outside, never Go's %v spelling.
func TestNumberStringIsNumberToString(t *testing.T) {
	for _, c := range []struct {
		in   float64
		want string
	}{
		{0, "0"},
		{math.Copysign(0, -1), "0"},
		{1, "1"},
		{-1, "-1"},
		{1.5, "1.5"},
		{0.1, "0.1"},
		{0.30000000000000004, "0.30000000000000004"},
		{100, "100"},
		{1000000, "1000000"},
		{1234567, "1234567"},
		{123456789012345680000, "123456789012345680000"},
		{1e20, "100000000000000000000"},
		{1e21, "1e+21"},
		{-1e21, "-1e+21"},
		{1.5e300, "1.5e+300"},
		{1.2345e25, "1.2345e+25"},
		{0.000001, "0.000001"},
		{0.0000012345, "0.0000012345"},
		{1e-7, "1e-7"},
		{1.5e-7, "1.5e-7"},
		{-2.5e-9, "-2.5e-9"},
		{5e-324, "5e-324"},
		{math.MaxFloat64, "1.7976931348623157e+308"},
		{math.NaN(), "NaN"},
		{math.Inf(1), "Infinity"},
		{math.Inf(-1), "-Infinity"},
	} {
		if got := NumberString(c.in); got != c.want {
			t.Errorf("NumberString(%v) = %q, want %q", c.in, got, c.want)
		}
	}
}

// A finite value reads back as itself, whichever notation it takes.
func TestNumberStringRoundTrips(t *testing.T) {
	x := uint64(88172645463325252)
	for i := 0; i < 20000; i++ {
		x ^= x << 13
		x ^= x >> 7
		x ^= x << 17
		f := math.Float64frombits(x)
		if math.IsNaN(f) || math.IsInf(f, 0) {
			continue
		}
		back, err := strconv.ParseFloat(NumberString(f), 64)
		if err != nil || back != f && f != 0 {
			t.Fatalf("NumberString(%v) = %q reads back as %v, %v", f, NumberString(f), back, err)
		}
	}
}
