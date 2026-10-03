package dagsched

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// The host memory bound (CRW-468) reads three numbers of the host and nothing else: MemAvailable, swap use and the some avg60 of the memory pressure (with the some avg10 beside it as information). These tests feed it fake /proc
// contents; none reads the live host.

// fakeProc writes a /proc root: meminfo and, when pressure is not empty, pressure/memory. An empty text leaves the file absent, as on a kernel without it.
func fakeProc(t *testing.T, meminfo, pressure string) string {
	t.Helper()
	root := t.TempDir()
	if meminfo != "" {
		if err := os.WriteFile(filepath.Join(root, "meminfo"), []byte(meminfo), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if pressure != "" {
		if err := os.MkdirAll(filepath.Join(root, "pressure"), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, "pressure", "memory"), []byte(pressure), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

// memInfo is a /proc/meminfo with the three fields the bound reads among others it ignores (the kernel prints kibibytes, labelled kB).
func memInfo(availableKiB, swapTotalKiB, swapFreeKiB int64) string {
	return fmt.Sprintf("MemTotal:       64395904 kB\nMemFree:         1234567 kB\nMemAvailable:   %d kB\nBuffers:          100000 kB\nSwapCached:            0 kB\nSwapTotal:      %d kB\nSwapFree:       %d kB\n", availableKiB, swapTotalKiB, swapFreeKiB)
}

// psi is a /proc/pressure/memory with the given some avg10 and avg60 (the bound judges the avg60).
func psi(avg10, avg60 string) string {
	return "some avg10=" + avg10 + " avg60=" + avg60 + " avg300=0.50 total=3098489\nfull avg10=0.00 avg60=0.00 avg300=0.00 total=2627867\n"
}

const kibPerGiB = 1 << 20

// Every command the CLI tests run reads the host through the environment: point them at a proc root that does not exist and drop the thresholds a developer's shell may carry, so no test of this package reads the live host or depends on it (a test that wants a host sets its own).
func init() {
	_ = os.Setenv(EnvHostProcRoot, "/nonexistent/crw-468-proc")
	for _, name := range []string{EnvHostMinAvailableGiB, EnvHostMaxSwapPercent, EnvHostMaxPressure} {
		_ = os.Unsetenv(name)
	}
}

func TestProcHostMemoryReadsMeminfoAndPressure(t *testing.T) {
	root := fakeProc(t, memInfo(40*kibPerGiB, 8*kibPerGiB, 6*kibPerGiB), psi("7.25", "3.50"))
	got := ReadHostMemory(root)
	if got.AvailableBytes == nil || *got.AvailableBytes != 40<<30 {
		t.Fatalf("available = %v, want 40 GiB (the kernel prints kibibytes)", got.AvailableBytes)
	}
	if got.SwapTotalBytes == nil || *got.SwapTotalBytes != 8<<30 || got.SwapFreeBytes == nil || *got.SwapFreeBytes != 6<<30 {
		t.Fatalf("swap total/free = %v/%v, want 8 GiB/6 GiB", got.SwapTotalBytes, got.SwapFreeBytes)
	}
	if got.PressureSomeAvg60 == nil || *got.PressureSomeAvg60 != 3.5 || got.PressureSomeAvg10 == nil || *got.PressureSomeAvg10 != 7.25 {
		t.Fatalf("pressure some avg60/avg10 = %v/%v, want 3.5 judged and 7.25 beside it (the some line, not the full line)", got.PressureSomeAvg60, got.PressureSomeAvg10)
	}
	if len(got.Unread) != 0 {
		t.Fatalf("unread = %v, want none", got.Unread)
	}
}

// An older kernel has no /proc/pressure/memory: that is no reading of the pressure, never a pressure.
func TestProcHostMemoryMissingPressureIsNoReading(t *testing.T) {
	root := fakeProc(t, memInfo(40*kibPerGiB, 8*kibPerGiB, 8*kibPerGiB), "")
	got := ReadHostMemory(root)
	if got.PressureSomeAvg60 != nil || got.PressureSomeAvg10 != nil {
		t.Fatalf("pressure = %v/%v, want no reading", got.PressureSomeAvg60, got.PressureSomeAvg10)
	}
	if len(got.Unread) != 1 || !strings.HasPrefix(got.Unread[0], "pressure: ") {
		t.Fatalf("unread = %v, want the pressure dimension named once with its cause", got.Unread)
	}
	verdict := HostMemoryBound{Sample: ReadHostMemory(root), Limits: DefaultHostMemoryLimits()}.Judge()
	if verdict.State != HostMemoryWithin || !reflect.DeepEqual(verdict.Unmeasured, []string{"pressure"}) || len(verdict.Exceeded) != 0 {
		t.Fatalf("verdict = %+v, want within with only the pressure unmeasured", verdict)
	}
}

func TestProcHostMemoryUnreadableFilesAreNoReading(t *testing.T) {
	cases := []struct {
		name, meminfo, pressure string
		unmeasured              []string
	}{
		{"no meminfo", "", psi("0.00", "1.00"), []string{"available", "swap"}},
		{"no MemAvailable line (a kernel before 3.14)", "MemTotal: 1 kB\nSwapTotal: 8 kB\nSwapFree: 8 kB\n", psi("0.00", "1.00"), []string{"available"}},
		{"a MemAvailable that is not a number", "MemAvailable: lots kB\nSwapTotal: 8 kB\nSwapFree: 8 kB\n", psi("0.00", "1.00"), []string{"available"}},
		{"a MemAvailable in an unknown unit", "MemAvailable: 5 GB\nSwapTotal: 8 kB\nSwapFree: 8 kB\n", psi("0.00", "1.00"), []string{"available"}},
		{"swap free above swap total", memInfo(40*kibPerGiB, 8, 9), psi("0.00", "1.00"), []string{"swap"}},
		{"no SwapFree line", "MemAvailable: 41943040 kB\nSwapTotal: 8 kB\n", psi("0.00", "1.00"), []string{"swap"}},
		{"a pressure file without a some line", memInfo(40*kibPerGiB, 8, 8), "full avg10=0.00 avg60=0.00 avg300=0.00 total=1\n", []string{"pressure"}},
		{"a some line without avg60, though it has an avg10", memInfo(40*kibPerGiB, 8, 8), "some avg10=0.00 avg300=0.00 total=1\n", []string{"pressure"}},
		{"a pressure avg60 that is not a number", memInfo(40*kibPerGiB, 8, 8), psi("0.00", "high"), []string{"pressure"}},
		{"a pressure avg60 below zero", memInfo(40*kibPerGiB, 8, 8), psi("0.00", "-1.00"), []string{"pressure"}},
		{"a pressure avg60 above 100", memInfo(40*kibPerGiB, 8, 8), psi("0.00", "101.00"), []string{"pressure"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			root := fakeProc(t, c.meminfo, c.pressure)
			verdict := HostMemoryBound{Sample: ReadHostMemory(root), Limits: DefaultHostMemoryLimits()}.Judge()
			if !reflect.DeepEqual(verdict.Unmeasured, c.unmeasured) {
				t.Fatalf("unmeasured = %v, want %v (verdict %+v)", verdict.Unmeasured, c.unmeasured, verdict)
			}
			if verdict.State == HostMemoryDeferring {
				t.Fatalf("a dimension nobody could read deferred a release: %+v", verdict)
			}
		})
	}
	t.Run("a pressure path that is a directory", func(t *testing.T) {
		root := fakeProc(t, memInfo(40*kibPerGiB, 8, 8), psi("0.00", "1.00"))
		if err := os.Remove(filepath.Join(root, "pressure", "memory")); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(filepath.Join(root, "pressure", "memory"), 0o700); err != nil {
			t.Fatal(err)
		}
		got := ReadHostMemory(root)
		if got.PressureSomeAvg60 != nil || len(got.Unread) != 1 {
			t.Fatalf("a read error of the pressure file: %+v", got)
		}
	})
}

// No swap at all is a measured zero use, not an unread dimension.
func TestProcHostMemoryWithoutSwapIsZeroUse(t *testing.T) {
	root := fakeProc(t, memInfo(40*kibPerGiB, 0, 0), psi("0.00", "0.00"))
	verdict := HostMemoryBound{Sample: ReadHostMemory(root), Limits: DefaultHostMemoryLimits()}.Judge()
	if used, ok := verdict.SwapUsedPercent(); !ok || used != 0 || len(verdict.Unmeasured) != 0 || verdict.State != HostMemoryWithin {
		t.Fatalf("swap used = %v/%v, verdict %+v, want a measured 0 percent", used, ok, verdict)
	}
}

func fixedMemory(available int64, swapTotal, swapFree int64, pressure float64) HostMemory {
	return HostMemory{AvailableBytes: &available, SwapTotalBytes: &swapTotal, SwapFreeBytes: &swapFree, PressureSomeAvg60: &pressure}
}

// The thresholds are strict: available is below its floor, swap and pressure are above their ceilings. A reading exactly at a threshold is within.
func TestHostMemoryJudgeThresholdBoundaries(t *testing.T) {
	limits := DefaultHostMemoryLimits()
	floor := limits.MinAvailableBytes
	cases := []struct {
		name      string
		available int64
		swapFree  int64   // of 20 GiB
		pressure  float64 // the some avg60
		state     string
		exceeded  []string
	}{
		{"healthy", 40 << 30, 20 << 30, 0, HostMemoryWithin, nil},
		{"available exactly at the floor", floor, 20 << 30, 0, HostMemoryWithin, nil},
		{"available one byte under the floor", floor - 1, 20 << 30, 0, HostMemoryDeferring, []string{"available"}},
		{"75 percent of the swap is within the default", 40 << 30, 5 << 30, 0, HostMemoryWithin, nil},
		{"swap exactly at 85 percent", 40 << 30, 3 << 30, 0, HostMemoryWithin, nil},
		{"swap one byte over 85 percent", 40 << 30, 3<<30 - 1, 0, HostMemoryDeferring, []string{"swap"}},
		{"pressure avg60 exactly at the ceiling", 40 << 30, 20 << 30, 10, HostMemoryWithin, nil},
		{"pressure avg60 just over the ceiling", 40 << 30, 20 << 30, 10.01, HostMemoryDeferring, []string{"pressure"}},
		{"all three over", 1 << 30, 0, 99, HostMemoryDeferring, []string{"available", "swap", "pressure"}},
		{"the incident window: swap full, reclaim storm", 2 << 30, 0, 45.5, HostMemoryDeferring, []string{"available", "swap", "pressure"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			verdict := HostMemoryBound{Sample: fixedMemory(c.available, 20<<30, c.swapFree, c.pressure), Limits: limits}.Judge()
			if verdict.State != c.state || !reflect.DeepEqual(verdict.Exceeded, c.exceeded) || len(verdict.Unmeasured) != 0 {
				t.Fatalf("verdict = %+v, want %s exceeding %v", verdict, c.state, c.exceeded)
			}
		})
	}
}

// Nothing readable is unmeasured: the bound records what was read and never claims the host is safe, and it defers nothing.
func TestHostMemoryJudgeUnmeasuredWhenNothingIsRead(t *testing.T) {
	verdict := HostMemoryBound{Sample: ReadHostMemory(filepath.Join(t.TempDir(), "absent")), Limits: DefaultHostMemoryLimits()}.Judge()
	if verdict.State != HostMemoryUnmeasured || len(verdict.Exceeded) != 0 || !reflect.DeepEqual(verdict.Unmeasured, []string{"available", "swap", "pressure"}) {
		t.Fatalf("verdict = %+v, want unmeasured in all three dimensions", verdict)
	}
}

// The documented defaults, and a detail that names every measured value, its threshold and the dimensions nobody read.
func TestHostMemoryDefaultsAndDetail(t *testing.T) {
	limits := DefaultHostMemoryLimits()
	if limits.MinAvailableBytes != 15<<30 || limits.MaxSwapPercent != 85 || limits.MaxPressureSomeAvg60 != 10 {
		t.Fatalf("defaults = %+v, want 15 GiB, 85 percent and an avg60 of 10", limits)
	}
	verdict := HostMemoryBound{Sample: fixedMemory(9<<30, 8<<30, 1<<30, 12.5), Limits: limits}.Judge()
	detail := verdict.Detail()
	for _, want := range []string{"available 9.00 GiB is under the 15.00 GiB floor", "swap use 87.50 percent is over the 85.00 percent ceiling", "pressure some avg60 12.50 is over the 10.00 ceiling"} {
		if !strings.Contains(detail, want) {
			t.Errorf("detail %q lacks %q", detail, want)
		}
	}
}

func TestHostMemoryFromEnvironment(t *testing.T) {
	env := func(m map[string]string) func(string) string { return func(k string) string { return m[k] } }
	t.Run("nothing set but the proc root: the defaults", func(t *testing.T) {
		root := fakeProc(t, memInfo(40*kibPerGiB, 8*kibPerGiB, 8*kibPerGiB), psi("0.00", "0.00"))
		bound, err := HostMemoryFromEnvironment(env(map[string]string{EnvHostProcRoot: root}))
		if err != nil || bound.Limits != DefaultHostMemoryLimits() || bound.LimitsFrom != LimitsDefault || bound.Sample.AvailableBytes == nil {
			t.Fatalf("bound = %+v err = %v", bound, err)
		}
	})
	t.Run("overrides and a proc root", func(t *testing.T) {
		root := fakeProc(t, memInfo(7*kibPerGiB, 8*kibPerGiB, 8*kibPerGiB), psi("0.00", "0.00"))
		bound, err := HostMemoryFromEnvironment(env(map[string]string{EnvHostProcRoot: root, EnvHostMinAvailableGiB: "6.5", EnvHostMaxSwapPercent: "80", EnvHostMaxPressure: "25"}))
		want := HostMemoryLimits{MinAvailableBytes: 13 << 29, MaxSwapPercent: 80, MaxPressureSomeAvg60: 25}
		if err != nil || bound.Limits != want || bound.LimitsFrom != LimitsEnvironment {
			t.Fatalf("bound = %+v err = %v, want %+v", bound, err, want)
		}
		if verdict := bound.Judge(); verdict.State != HostMemoryWithin {
			t.Fatalf("7 GiB available under a floor of 6.5 GiB: %+v", verdict)
		}
	})
	t.Run("the old pressure variable is not read", func(t *testing.T) {
		bound, err := HostMemoryFromEnvironment(env(map[string]string{EnvHostProcRoot: t.TempDir(), "CRW_DAG_HOST_MAX_PRESSURE_SOME_AVG10": "50"}))
		if err != nil || bound.Limits != DefaultHostMemoryLimits() || bound.LimitsFrom != LimitsDefault {
			t.Fatalf("bound = %+v err = %v, want the defaults", bound, err)
		}
	})
	t.Run("a value that is not usable names its variable", func(t *testing.T) {
		for name, value := range map[string]string{
			EnvHostMinAvailableGiB: "lots", EnvHostMaxSwapPercent: "101", EnvHostMaxPressure: "-1",
		} {
			_, err := HostMemoryFromEnvironment(env(map[string]string{name: value}))
			if err == nil || !strings.Contains(err.Error(), name) {
				t.Errorf("%s=%q: err = %v, want a refusal that names the variable", name, value, err)
			}
		}
		for _, bad := range []string{"NaN", "Inf", "-0.5"} {
			if _, err := HostMemoryFromEnvironment(env(map[string]string{EnvHostMinAvailableGiB: bad})); err == nil {
				t.Errorf("%s=%q was accepted", EnvHostMinAvailableGiB, bad)
			}
		}
	})
}

// The pressure is judged on the some avg60; the avg10 is recorded beside it and decides nothing, and an avg10 that cannot be read is no reading of its own, not a lost avg60.
func TestHostMemoryJudgesPressureOnAvg60(t *testing.T) {
	for _, c := range []struct{ name, file, state, avg10 string }{
		{"a spike of avg10 alone holds nothing", psi("50.00", "1.00"), HostMemoryWithin, "50.00"},
		{"avg60 exactly at the ceiling is within", psi("0.00", "10.00"), HostMemoryWithin, "0.00"},
		{"avg60 over the ceiling holds although avg10 is calm", psi("0.00", "10.01"), HostMemoryDeferring, "0.00"},
		{"a missing avg10 leaves the avg60 verdict alone", "some avg60=10.01 avg300=0.50 total=1\n", HostMemoryDeferring, ""},
		{"an avg10 out of range leaves the avg60 verdict alone", psi("101.00", "1.00"), HostMemoryWithin, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			verdict := HostMemoryBound{Sample: ReadHostMemory(fakeProc(t, memInfo(40*kibPerGiB, 20*kibPerGiB, 20*kibPerGiB), c.file)), Limits: DefaultHostMemoryLimits()}.Judge()
			info := asJSON(t, verdict.object())["measured"].(map[string]any)["pressure_some_avg10"]
			if verdict.State != c.state || len(verdict.Sample.Unread) != 0 || (c.avg10 == "") != (info == nil) || (c.avg10 != "" && fmt.Sprintf("%.2f", info) != c.avg10) {
				t.Fatalf("verdict = %+v, info %v, want %s with the avg10 %q beside the avg60 and nothing unread", verdict, info, c.state, c.avg10)
			}
		})
	}
}
