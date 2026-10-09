//go:build dev

package laneparity

import (
	"strings"
	"testing"
	"time"
)

func ms(n ...int) []time.Duration {
	var out []time.Duration
	for _, v := range n {
		out = append(out, time.Duration(v)*time.Millisecond)
	}
	return out
}

func TestPercentile_isTheNearestRank(t *testing.T) {
	s := ms(50, 10, 40, 20, 30, 60, 70, 80, 90, 100)
	for p, want := range map[float64]time.Duration{50: 50 * time.Millisecond, 95: 100 * time.Millisecond, 10: 10 * time.Millisecond, 100: 100 * time.Millisecond} {
		if got := Percentile(s, p); got != want {
			t.Errorf("p%v = %s, want %s", p, got, want)
		}
	}
	if got := Percentile(nil, 95); got != 0 {
		t.Errorf("no samples: %s", got)
	}
	if got := Percentile(ms(7), 50); got != 7*time.Millisecond {
		t.Errorf("one sample: %s", got)
	}
	in := ms(3, 1, 2)
	Percentile(in, 50)
	if in[0] != 3*time.Millisecond {
		t.Error("Percentile sorted its argument")
	}
}

func TestJudge(t *testing.T) {
	for _, c := range []struct {
		name    string
		timeout int
		goS, ts []time.Duration
		ok      bool
		reason  string
	}{
		{"faster than the oracle and inside half the timeout", 10, ms(5, 6, 7), ms(40, 50, 60), true, ""},
		{"equal p95 passes", 10, ms(50), ms(50), true, ""},
		{"slower than the oracle", 10, ms(5, 6, 70), ms(40, 50, 60), false, "above the TS p95"},
		{"past half the timeout", 10, ms(5100), ms(9000), false, "half of the 10s timeout"},
		{"exactly half the timeout", 10, ms(5000), nil, true, ""},
		{"no oracle: the timeout alone", 10, ms(4000), nil, true, ""},
		{"no oracle, too slow", 10, ms(6000), nil, false, "half of the 10s timeout"},
		{"no samples", 10, nil, ms(1), false, "no Go sample"},
		{"no declared timeout", 0, ms(1), ms(5), false, "no declared timeout"},
	} {
		t.Run(c.name, func(t *testing.T) {
			l := Judge("leg", "fixture", c.timeout, c.goS, c.ts)
			if l.OK != c.ok || !strings.Contains(l.Reason, c.reason) {
				t.Errorf("OK=%v reason=%q, want OK=%v reason containing %q", l.OK, l.Reason, c.ok, c.reason)
			}
			if l.Oracle != (len(c.ts) > 0) || l.TimeoutMs != c.timeout*1000 || l.Runs != len(c.goS) {
				t.Errorf("fields: %+v", l)
			}
		})
	}
}

func TestSettle_aFailureUnderAnOverloadedHostIsInconclusiveUnlessStrict(t *testing.T) {
	failing := Judge("leg", "f", 10, ms(80), ms(40))
	if failing.OK {
		t.Fatal("setup: the verdict should fail")
	}
	for _, c := range []struct {
		name         string
		v            Latency
		load         float64
		known        bool
		cpus         int
		strict       bool
		ok, inconcl  bool
		reasonSubstr string
	}{
		{"overloaded", failing, 44, true, 20, false, true, true, "host load 44.0 is above its 20 CPUs"},
		{"load at the CPU count", failing, 20, true, 20, false, false, false, "above the TS p95"},
		{"quiet host", failing, 3, true, 20, false, false, false, "above the TS p95"},
		{"load unknown", failing, 0, false, 20, false, false, false, "above the TS p95"},
		{"strict", failing, 44, true, 20, true, false, false, "above the TS p95"},
		{"a pass stays a pass", Judge("leg", "f", 10, ms(5), ms(40)), 44, true, 20, false, true, false, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			got := Settle(c.v, c.load, c.known, c.cpus, c.strict)
			if got.OK != c.ok || got.Inconclusive != c.inconcl || !strings.Contains(got.Reason, c.reasonSubstr) {
				t.Errorf("%+v", got)
			}
			if c.known && got.Load != c.load {
				t.Errorf("load %v", got.Load)
			}
		})
	}
}
