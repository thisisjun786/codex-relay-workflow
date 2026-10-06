package dagsched

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

// Literal names and decoded JSON keep these regressions runnable before the new signal exists.
const hostMemoryFullAvg10Environment = "CRW_DAG_HOST_MAX_PRESSURE_FULL_AVG10"

func hostMemoryPressureFile(some10, some60, full10, full60 string) string {
	return fmt.Sprintf("some avg10=%s avg60=%s avg300=0.00 total=1\nfull avg10=%s avg60=%s avg300=0.00 total=1\n", some10, some60, full10, full60)
}

func hostMemoryProcBound(t *testing.T, root string, overrides map[string]string) *HostMemoryBound {
	t.Helper()
	bound, err := HostMemoryFromEnvironment(func(key string) string {
		if key == EnvHostProcRoot {
			return root
		}
		return overrides[key]
	})
	if err != nil {
		t.Fatal(err)
	}
	return bound
}

// Log-shaped values are synthetic: the logs measure avg10, not the existing some avg60.
// The precise 18:46 incident is absent; low-available/high-pressure cases model its described shape.
func TestHostMemoryPressureEvidenceShapes(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name                   string
		availableMiB, usedMiB  int64
		some10, some60, full10 string
		state                  string
		exceeded               []string
	}{
		{"healthy 05:42 shape", 40 * 1024, 8180, "0.51", "0.00", "0.51", HostMemoryWithin, nil},
		{"healthy 06:24 shape", 50 * 1024, 7966, "0.00", "0.00", "0.00", HostMemoryWithin, nil},
		{"full swap and low available", 9 * 1024, 8192, "0.00", "0.00", "0.00", HostMemoryDeferring, []string{"available"}},
		{"full swap and high some pressure", 40 * 1024, 8192, "19.00", "12.50", "0.00", HostMemoryDeferring, []string{"pressure"}},
		{"recorded full stall shape with ample available", 23 * 1024, 4934, "8.51", "0.00", "7.42", HostMemoryDeferring, []string{"pressure_full"}},
		{"full at the ceiling", 40 * 1024, 8192, "8.00", "0.00", "5.00", HostMemoryWithin, nil},
		{"full just over the ceiling", 40 * 1024, 8192, "8.00", "0.00", "5.01", HostMemoryDeferring, []string{"pressure_full"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			root := fakeProc(t, memInfo(c.availableMiB*1024, 8192*1024, (8192-c.usedMiB)*1024), hostMemoryPressureFile(c.some10, c.some60, c.full10, "0.00"))
			f := newFixture(t)
			f.threeNodes("host-pressure")
			f.sched.Host = hostMemoryProcBound(t, root, nil)
			r := f.read("host-pressure")
			v := r.Pass.HostMemory
			if v.State != c.state || !reflect.DeepEqual(v.Exceeded, c.exceeded) {
				t.Fatalf("verdict %+v, want %s exceeding %v", v, c.state, c.exceeded)
			}
			if (c.state == HostMemoryWithin && len(r.Ready) != 3) || (c.state == HostMemoryDeferring && len(r.Ready) != 0) {
				t.Fatalf("ready=%v for host %s", readyIDs(r), v.State)
			}
			for _, n := range r.Nodes {
				if c.state == HostMemoryDeferring && n.Reason != DeferHostMemory {
					t.Fatalf("node %+v lacks the host reason", n)
				}
			}
		})
	}
}

func TestHostMemoryPressureUnreadSignals(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name, pressure string
		unmeasured     []string
	}{
		{"missing pressure", "", []string{"pressure", "pressure_full"}},
		{"missing full", "some avg10=0.00 avg60=0.00 total=1\n", []string{"pressure_full"}},
		{"malformed full", hostMemoryPressureFile("0", "0", "bad", "0"), []string{"pressure_full"}},
		{"non-finite full", hostMemoryPressureFile("0", "0", "NaN", "0"), []string{"pressure_full"}},
		{"full out of range", hostMemoryPressureFile("0", "0", "101", "0"), []string{"pressure_full"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			v := hostMemoryProcBound(t, fakeProc(t, memInfo(40*kibPerGiB, 8*kibPerGiB, 0), c.pressure), nil).Judge()
			m := asJSON(t, v.object())["measured"].(map[string]any)
			full, present := m["pressure_full_avg10"]
			if v.State != HostMemoryWithin || !reflect.DeepEqual(v.Unmeasured, c.unmeasured) || !present || full != nil {
				t.Fatalf("verdict %+v, full=%v/%v, want unread %v", v, full, present, c.unmeasured)
			}
			if !strings.Contains(strings.Join(v.Sample.Unread, ";"), "pressure_full:") {
				t.Fatal("missing full signal has no recorded cause")
			}
		})
	}
	t.Run("missing full does not erase measured some pressure", func(t *testing.T) {
		v := hostMemoryProcBound(t, fakeProc(t, memInfo(40*kibPerGiB, 8*kibPerGiB, 0), "some avg60=12.50 total=1\n"), nil).Judge()
		if v.State != HostMemoryDeferring || !reflect.DeepEqual(v.Exceeded, []string{"pressure"}) || !reflect.DeepEqual(v.Unmeasured, []string{"pressure_full"}) {
			t.Fatalf("independent pressure signals: %+v", v)
		}
	})
	if v := hostMemoryProcBound(t, t.TempDir(), nil).Judge(); v.State != HostMemoryUnmeasured || len(v.Unmeasured) != 4 {
		t.Fatalf("all unread: %+v", v)
	}
}

func TestHostMemoryPressureConfiguration(t *testing.T) {
	t.Parallel()
	root := fakeProc(t, memInfo(40*kibPerGiB, 8*kibPerGiB, 0), hostMemoryPressureFile("8", "0", "7.42", "99"))
	for _, c := range []struct {
		name, value, state string
		ceiling            float64
	}{
		{"unset", "", HostMemoryDeferring, 5},
		{"empty", "  ", HostMemoryDeferring, 5},
		{"override", "8", HostMemoryWithin, 8},
		{"disabled", "100", HostMemoryWithin, 100},
	} {
		t.Run(c.name, func(t *testing.T) {
			v := hostMemoryProcBound(t, root, map[string]string{hostMemoryFullAvg10Environment: c.value}).Judge()
			printed := asJSON(t, v.object())
			if v.State != c.state || printed["limits"].(map[string]any)["max_pressure_full_avg10"] != c.ceiling {
				t.Fatalf("value %q: %+v, limits=%v", c.value, v, printed["limits"])
			}
		})
	}
	for _, value := range []string{"bad", "NaN", "Inf", "-1", "101"} {
		_, err := HostMemoryFromEnvironment(func(key string) string {
			if key == EnvHostProcRoot {
				return root
			}
			if key == hostMemoryFullAvg10Environment {
				return value
			}
			return ""
		})
		if err == nil || !strings.Contains(err.Error(), hostMemoryFullAvg10Environment) {
			t.Errorf("invalid %q: %v", value, err)
		}
	}
	calm := fakeProc(t, memInfo(40*kibPerGiB, 8*kibPerGiB, 0), hostMemoryPressureFile("0", "0", "0", "99"))
	if v := hostMemoryProcBound(t, calm, map[string]string{EnvHostMaxSwapPercent: "85"}).Judge(); !reflect.DeepEqual(v.Exceeded, []string{"swap"}) {
		t.Fatalf("explicit occupancy override: %+v", v)
	}
	if v := hostMemoryProcBound(t, calm, nil).Judge(); v.State != HostMemoryWithin {
		t.Fatalf("full avg60 alone must not hold: %+v", v)
	}
}

func TestHostMemoryPressurePersistenceAndDigest(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.threeNodes("host-pressure")
	root := fakeProc(t, memInfo(40*kibPerGiB, 8*kibPerGiB, 0), hostMemoryPressureFile("8.51", "0", "7.42", "0"))
	f.sched.Host = hostMemoryProcBound(t, root, nil)
	first, seq := f.recordPass("host-pressure")
	var saved map[string]any
	if err := json.Unmarshal([]byte(f.hostRows("host-pressure")[seq].doc), &saved); err != nil {
		t.Fatal(err)
	}
	if saved["measured"].(map[string]any)["pressure_full_avg10"] != 7.42 || saved["limits"].(map[string]any)["max_pressure_full_avg10"] != float64(5) || !reflect.DeepEqual(saved["exceeded"], []any{"pressure_full"}) {
		t.Fatalf("persisted full signal: %v", saved)
	}
	other := fakeProc(t, memInfo(40*kibPerGiB, 8*kibPerGiB, 0), hostMemoryPressureFile("8.51", "0", "7.43", "0"))
	f.sched.Host = hostMemoryProcBound(t, other, nil)
	if f.read("host-pressure").InputDigest == first.InputDigest {
		t.Fatal("full-only change is absent from input digest")
	}
}

// sequential: t.Setenv(EnvHostProcRoot) is process-wide.
func TestHostMemoryPressureReleaseAndCLI(t *testing.T) {
	k := newReleaseKit(t)
	releasePlan(k.fixture, "host-pressure")
	k.sched.Host = hostMemoryProcBound(t, fakeProc(t, memInfo(40*kibPerGiB, 8*kibPerGiB, 0), hostMemoryPressureFile("8", "0", "7.42", "0")), nil)
	_, err := k.release("host-pressure", "A")
	if refusalReason(err) != "capacity_exhausted" || !strings.Contains(err.Error(), "pressure full avg10 7.42") || k.rows() != (rowCounts{}) {
		t.Fatalf("held release: %v, rows %+v", err, k.rows())
	}
	if created, _ := k.host.counts(); created != 0 {
		t.Fatal("a pressured fake host was asked to create a child")
	}
	k.sched.Host = hostMemoryProcBound(t, fakeProc(t, memInfo(40*kibPerGiB, 8*kibPerGiB, 0), hostMemoryPressureFile("0", "0", "0", "0")), nil)
	if result := k.mustRelease("host-pressure", "A"); !result.Bound {
		t.Fatalf("healthy full-swap release: %+v", result)
	}

	state, _ := cliState(t)
	t.Setenv(EnvHostProcRoot, fakeProc(t, memInfo(40*kibPerGiB, 8*kibPerGiB, 0), hostMemoryPressureFile("0", "0", "0", "0")))
	out, stderr, code := crwRun(t, state, nil, "dag-ready", "--plan", "p1")
	if code != 0 || stderr != "" || len(parseOut(t, out)["ready"].([]any)) != 1 {
		t.Fatalf("healthy CLI stdout=%s stderr=%s exit=%d", out, stderr, code)
	}
	t.Setenv(hostMemoryFullAvg10Environment, "NaN")
	out, stderr, code = crwRun(t, state, nil, "dag-ready", "--plan", "p1")
	if code != 4 || !strings.Contains(out+stderr, hostMemoryFullAvg10Environment) {
		t.Fatalf("invalid full threshold stdout=%s stderr=%s exit=%d", out, stderr, code)
	}
}
