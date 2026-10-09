//go:build dev

package laneparity

import (
	"bytes"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport"
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

// rootStarting is a generated root whose every command starts crw, fired and timed as the build under test.
func rootStarting(t *testing.T, crw string) FireOptions {
	t.Helper()
	root := repoRoot(t)
	legs, err := ExpectedLegs(root)
	if err != nil {
		t.Fatal(err)
	}
	plugin := filepath.Join(t.TempDir(), "crw")
	if err := GeneratePluginRoot(plugin, filepath.Join(root, "plugins", "crw"), crw, legs); err != nil {
		t.Fatal(err)
	}
	return FireOptions{Root: root, CRW: crw, Plugin: plugin, Scratch: t.TempDir()}
}

// wrapper writes an executable named crw that runs body (the real build is "$REAL").
func wrapper(t *testing.T, body string) string {
	t.Helper()
	real, err := crwUnderTest()
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "bin", "crw")
	script := "#!/bin/sh\nREAL='" + real + "'\n" + body + "\n"
	if err := testsupport.WriteProgram(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// A hook that fails at once is faster than a working one: the cell must not pass it. Each declared
// command is a well-formed registration of the leg; what it starts is what fails.
func TestMeasureLatency_aFailingHookIsNotAFastPass(t *testing.T) {
	good := latencyOf(t, fireFixture(t), mapLeg)
	if !good.OK {
		t.Fatalf("the working hook does not pass: %+v", good)
	}
	for name, setup := range map[string]func(*testing.T) FireOptions{
		"exits nonzero": func(t *testing.T) FireOptions {
			return rootStarting(t, wrapper(t, `"$REAL" "$@"; exit 3`))
		},
		"another executable": func(t *testing.T) FireOptions {
			real, err := crwUnderTest()
			if err != nil {
				t.Fatal(err)
			}
			other, err := exec.LookPath("false")
			if err != nil {
				t.Skip("no false on PATH")
			}
			started := filepath.Join(t.TempDir(), "bin", "crw")
			if err := testsupport.CopyBinary(other, started); err != nil {
				t.Fatal(err)
			}
			o := rootStarting(t, started)
			o.CRW = real
			return o
		},
		"killed": func(t *testing.T) FireOptions {
			return rootStarting(t, wrapper(t, `kill -9 $$`))
		},
	} {
		t.Run(name, func(t *testing.T) {
			got := latencyOf(t, setup(t), mapLeg)
			if got.OK || got.Inconclusive || got.Skipped || !got.Broken {
				t.Fatalf("a failing hook passed the latency cell, or failed it for another reason: %+v", got)
			}
			t.Logf("%s", got.Reason)
		})
	}
}

// The verdict holds a leg to half the timeout the root declares for it, not to the table's: a
// registration of CRW's own passes the registration cell with any positive timeout.
func TestMeasureLatency_judgesAgainstTheDeclaredTimeout(t *testing.T) {
	o := rootStarting(t, wrapper(t, `/bin/sleep 0.6; exec "$REAL" "$@"`))
	if got := latencyOf(t, o, GitHubPostLeg); !got.OK || got.TimeoutMs != 10000 {
		t.Fatalf("a 0.6s hook under a 10s timeout: %+v", got)
	}
	path := filepath.Join(o.Plugin, githubPostFile)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(strings.Replace(string(raw), `"timeout": 10`, `"timeout": 1`, 1)), 0o644); err != nil {
		t.Fatal(err)
	}
	var out, errs bytes.Buffer
	if code := Run([]string{"registration", "--plugin", o.Plugin}, &out, &errs); code != 0 {
		t.Fatalf("setup: the shortened timeout must still register, exit %d: %s", code, out.String())
	}
	got := latencyOf(t, o, GitHubPostLeg)
	if got.OK || got.TimeoutMs != 1000 || !strings.Contains(got.Reason, "half of the 1s timeout") {
		t.Fatalf("a 0.6s hook under a declared 1s timeout passed or was judged against another timeout: %+v", got)
	}
}

func TestPrintLatency_countsTheLegsThatNeededAnotherAttempt(t *testing.T) {
	lat := []Latency{{Leg: "a", OK: true, Attempts: 1}, {Leg: "b", OK: true, Attempts: 2}, {Leg: "c", OK: true, Attempts: 3}, {Leg: "d", Skipped: true, OK: true}}
	var out bytes.Buffer
	printLatency(&out, lat)
	if !strings.Contains(out.String(), "3/3 legs pass") || !strings.Contains(out.String(), "1 on the first attempt, 2 on a later one") {
		t.Errorf("%s", out.String())
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
