//go:build dev

package laneparity

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
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

// editDeclaration replaces text in the hook files of a generated plugin root (the JSON spelling).
func editDeclaration(t *testing.T, plugin, from, to string) {
	t.Helper()
	hit := false
	err := filepath.WalkDir(filepath.Join(plugin, "wiring"), func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".json") {
			return err
		}
		raw, err := os.ReadFile(path)
		if err != nil || !strings.Contains(string(raw), from) {
			return err
		}
		hit = true
		return os.WriteFile(path, []byte(strings.ReplaceAll(string(raw), from, to)), 0o644)
	})
	if err != nil || !hit {
		t.Fatalf("editing %q: hit %v, %v", from, hit, err)
	}
}

const mapLeg = "session-start-announcing-map-affordance"

func latencyOf(t *testing.T, o FireOptions, leg string) Latency {
	t.Helper()
	lat, err := MeasureLatency(LatencyOptions{Root: o.Root, CRW: o.CRW, Plugin: o.Plugin, Scratch: o.Scratch, Runs: 1, Attempts: 1, Strict: true,
		Only: regexp.MustCompile("^" + regexp.QuoteMeta(leg) + "$")})
	if err != nil {
		return Latency{Reason: "error: " + err.Error()}
	}
	if len(lat) != 1 {
		t.Fatalf("%d legs timed", len(lat))
	}
	return lat[0]
}

// A hook that fails at once is faster than a working one: the cell must not pass it.
func TestMeasureLatency_aFailingHookIsNotAFastPass(t *testing.T) {
	good := latencyOf(t, fireFixture(t), mapLeg)
	if !good.OK {
		t.Fatalf("the working hook does not pass: %+v", good)
	}
	for name, edit := range map[string]func(*testing.T, string, string){
		"exits nonzero": func(t *testing.T, plugin, crw string) {
			editDeclaration(t, plugin, "--leg "+mapLeg+`"`, "--leg "+mapLeg+`; exit 3"`)
		},
		"another executable": func(t *testing.T, plugin, crw string) {
			editDeclaration(t, plugin, `\"`+crw+`\" hook session-start --leg `+mapLeg, "/bin/false hook session-start --leg "+mapLeg)
		},
		"killed": func(t *testing.T, plugin, crw string) {
			editDeclaration(t, plugin, "--leg "+mapLeg+`"`, "--leg "+mapLeg+`; kill -9 $$"`)
		},
	} {
		t.Run(name, func(t *testing.T) {
			o := fireFixture(t)
			edit(t, o.Plugin, o.CRW)
			got := latencyOf(t, o, mapLeg)
			if got.OK || got.Inconclusive || got.Skipped {
				t.Fatalf("a failing hook passed the latency cell: %+v", got)
			}
		})
	}
}

// A leg declared under another event, or not at all, is not a leg that can be timed.
func TestMeasureLatency_aLegWithNoDeclaredCommandFails(t *testing.T) {
	o := fireFixture(t)
	editDeclaration(t, o.Plugin, "--leg "+mapLeg+`"`, "--leg other-leg"+`"`)
	if got := latencyOf(t, o, mapLeg); got.OK {
		t.Fatalf("passed: %+v", got)
	}
}
