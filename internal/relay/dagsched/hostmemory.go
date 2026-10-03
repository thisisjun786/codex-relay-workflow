package dagsched

import (
	"errors"
	"fmt"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
)

// The host memory bound (CRW-468). A release starts one more child on a host other children already load, and the only bound used to be a number of slots. The bound reads the host once per command
// (MemAvailable, the swap in use, the some avg10 of the memory pressure); while one reading is over its threshold every candidate is deferred (defer:host_memory) and dag-release refuses a new
// release. A child that runs is not touched. A reading that cannot be had is recorded as unmeasured, never read as safe or as pressure. Only dag-ready and dag-release carry it (useHostMemory), so
// dag-progress and dag-restart stay functions of the store.

// The environment of those two commands; an empty value is unset. Docs: docs/relay/dag-scheduler.md, "Host memory".
const (
	EnvHostProcRoot        = "CRW_DAG_HOST_PROC_ROOT"               // where meminfo and pressure/memory are read (default /proc)
	EnvHostMinAvailableGiB = "CRW_DAG_HOST_MIN_AVAILABLE_GIB"       // floor of MemAvailable in GiB (default 15; 0 never trips)
	EnvHostMaxSwapPercent  = "CRW_DAG_HOST_MAX_SWAP_PERCENT"        // ceiling of the swap in use, percent of SwapTotal (default 50; 100 never trips)
	EnvHostMaxPressure     = "CRW_DAG_HOST_MAX_PRESSURE_SOME_AVG60" // ceiling of the some avg10 of the memory pressure (default 10; 100 never trips)
)

// The states of the bound (within: something was read and nothing is over; deferring: something is over; unmeasured: nothing could be read), where the limits came from, and the dimensions.
const (
	HostMemoryWithin     = "within"
	HostMemoryDeferring  = "deferring"
	HostMemoryUnmeasured = "unmeasured"
	LimitsDefault        = "default"
	LimitsEnvironment    = "environment"
	hostAvailable        = "available"
	hostSwap             = "swap"
	hostPressure         = "pressure"
	gib                  = 1 << 30
)

// HostMemory is one sample of the host. A nil field is a dimension nobody could read (Unread says why, "<dimension>: <cause>"); the swap is read only when both its total and its free bytes are.
type HostMemory struct {
	AvailableBytes, SwapTotalBytes, SwapFreeBytes *int64
	PressureSomeAvg60, PressureSomeAvg10          *float64
	Unread                                        []string
}

// HostMemoryLimits are the thresholds, all strict: available under MinAvailableBytes, swap use over MaxSwapPercent or pressure over MaxPressureSomeAvg60 holds releases.
type HostMemoryLimits struct {
	MinAvailableBytes                    int64
	MaxSwapPercent, MaxPressureSomeAvg60 float64
}

// DefaultHostMemoryLimits are the starting values: 15 GiB available, half of the swap, a some avg10 of 10.
func DefaultHostMemoryLimits() HostMemoryLimits {
	return HostMemoryLimits{MinAvailableBytes: 15 * gib, MaxSwapPercent: 50, MaxPressureSomeAvg60: 10}
}

// HostMemoryBound is the sample a command took and the limits it judges it by; Scheduler.Host nil means no bound.
type HostMemoryBound struct {
	Sample     HostMemory
	Limits     HostMemoryLimits
	LimitsFrom string // default | environment
}

// HostMemoryVerdict is what a bound says of its sample; Exceeded and Unmeasured name dimensions in the order available, swap, pressure.
type HostMemoryVerdict struct {
	State                string
	Limits               HostMemoryLimits
	LimitsFrom           string
	Sample               HostMemory
	Exceeded, Unmeasured []string
}

// ReadHostMemory samples the host from a proc root ("" is /proc): meminfo and pressure/memory, nothing else. It never fails; what cannot be read is a dimension without a reading, with its cause.
func ReadHostMemory(root string) HostMemory {
	if root == "" {
		root = "/proc"
	}
	var m HostMemory
	unread := func(dimension, cause string) { m.Unread = append(m.Unread, dimension+": "+cause) }
	if info, err := os.ReadFile(filepath.Join(root, "meminfo")); err != nil {
		unread(hostAvailable, causeOf(err))
		unread(hostSwap, causeOf(err))
	} else {
		fields := meminfoFields(string(info))
		total, haveTotal := fields["SwapTotal"]
		free, haveFree := fields["SwapFree"]
		if v, ok := fields["MemAvailable"]; ok {
			m.AvailableBytes = &v
		} else {
			unread(hostAvailable, "MemAvailable is missing or not in kB")
		}
		switch {
		case !haveTotal || !haveFree:
			unread(hostSwap, "SwapTotal or SwapFree is missing or not in kB")
		case free > total:
			unread(hostSwap, "SwapFree is above SwapTotal")
		default:
			m.SwapTotalBytes, m.SwapFreeBytes = &total, &free
		}
	}
	raw, err := os.ReadFile(filepath.Join(root, "pressure", "memory"))
	if v, ok := somePressure(string(raw), "avg10"); err == nil && ok {
		m.PressureSomeAvg60 = &v
	} else if errors.Is(err, fs.ErrNotExist) {
		unread(hostPressure, "no memory pressure file (a kernel without it)")
	} else if err != nil {
		unread(hostPressure, causeOf(err))
	} else {
		unread(hostPressure, "no usable some avg10")
	}
	return m
}

// causeOf is why a read failed, without the path: a recorded reading says what happened, not where the root was.
func causeOf(err error) string {
	var pathErr *fs.PathError
	if errors.As(err, &pathErr) {
		return pathErr.Err.Error()
	}
	return err.Error()
}

// meminfoFields are the fields of meminfo that are a whole number of kB, in bytes; any other line (another unit, no number) is left out and so reads as missing.
func meminfoFields(text string) map[string]int64 {
	out := map[string]int64{}
	for _, line := range strings.Split(text, "\n") {
		key, rest, ok := strings.Cut(line, ":")
		parts := strings.Fields(rest)
		if !ok || len(parts) != 2 || parts[1] != "kB" {
			continue
		}
		if n, err := strconv.ParseInt(parts[0], 10, 64); err == nil && n >= 0 && n <= math.MaxInt64/1024 {
			out[strings.TrimSpace(key)] = n * 1024
		}
	}
	return out
}

// somePressure is the avg10 of the "some" line of the memory pressure file.
func somePressure(text, window string) (float64, bool) {
	for _, line := range strings.Split(text, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 || fields[0] != "some" {
			continue
		}
		for _, f := range fields[1:] {
			if value, ok := strings.CutPrefix(f, window+"="); ok {
				v, err := strconv.ParseFloat(value, 64)
				return v, err == nil && v >= 0 && v <= 100
			}
		}
	}
	return 0, false
}

// SwapUsedPercent is the share of the swap in use; no swap at all (SwapTotal 0) is a measured zero.
func (v HostMemoryVerdict) SwapUsedPercent() (float64, bool) {
	total, free := v.Sample.SwapTotalBytes, v.Sample.SwapFreeBytes
	switch {
	case total == nil || free == nil:
		return 0, false
	case *total == 0:
		return 0, true
	}
	return float64(*total-*free) * 100 / float64(*total), true
}

// Judge compares the sample with the limits. It does no I/O.
func (b HostMemoryBound) Judge() HostMemoryVerdict {
	v := HostMemoryVerdict{Limits: b.Limits, LimitsFrom: b.LimitsFrom, Sample: b.Sample}
	over := func(dimension string, read, exceeded bool) {
		switch {
		case !read:
			v.Unmeasured = append(v.Unmeasured, dimension)
		case exceeded:
			v.Exceeded = append(v.Exceeded, dimension)
		}
	}
	m := b.Sample
	used, haveSwap := v.SwapUsedPercent()
	over(hostAvailable, m.AvailableBytes != nil, m.AvailableBytes != nil && *m.AvailableBytes < b.Limits.MinAvailableBytes)
	over(hostSwap, haveSwap, haveSwap && used > b.Limits.MaxSwapPercent)
	over(hostPressure, m.PressureSomeAvg60 != nil, m.PressureSomeAvg60 != nil && *m.PressureSomeAvg60 > b.Limits.MaxPressureSomeAvg60)
	switch {
	case len(v.Exceeded) > 0:
		v.State = HostMemoryDeferring
	case len(v.Unmeasured) == 3:
		v.State = HostMemoryUnmeasured
	default:
		v.State = HostMemoryWithin
	}
	return v
}

// Detail is the sentence a held node carries: every dimension over its threshold with the value and the threshold.
func (v HostMemoryVerdict) Detail() string {
	var parts []string
	for _, d := range v.Exceeded {
		switch used, _ := v.SwapUsedPercent(); d {
		case hostAvailable:
			parts = append(parts, fmt.Sprintf("available %.2f GiB is under the %.2f GiB floor", float64(*v.Sample.AvailableBytes)/gib, float64(v.Limits.MinAvailableBytes)/gib))
		case hostSwap:
			parts = append(parts, fmt.Sprintf("swap use %.2f percent is over the %.2f percent ceiling", used, v.Limits.MaxSwapPercent))
		case hostPressure:
			parts = append(parts, fmt.Sprintf("pressure some avg60 %.2f is over the %.2f ceiling", *v.Sample.PressureSomeAvg60, v.Limits.MaxPressureSomeAvg60))
		}
	}
	return "the host is short of memory, so no new release is made: " + strings.Join(parts, "; ")
}

func optionalInt(p *int64) any {
	if p == nil {
		return nil
	}
	return *p
}

// object is the verdict as the reading prints it (pass.host_memory) and a recorded pass keeps it (dag_pass_host_memory); the input digest covers it.
func (v HostMemoryVerdict) object() contract.OrderedObject {
	var used, avg60, avg10 any
	if percent, ok := v.SwapUsedPercent(); ok {
		used = percent
	}
	if v.Sample.PressureSomeAvg60 != nil {
		avg60 = *v.Sample.PressureSomeAvg60
	}
	if v.Sample.PressureSomeAvg10 != nil {
		avg10 = *v.Sample.PressureSomeAvg10
	}
	limits := contract.OrderedObject{{Key: "min_available_bytes", Value: v.Limits.MinAvailableBytes}, {Key: "max_swap_percent", Value: v.Limits.MaxSwapPercent}, {Key: "max_pressure_some_avg60", Value: v.Limits.MaxPressureSomeAvg60}}
	measured := contract.OrderedObject{{Key: "available_bytes", Value: optionalInt(v.Sample.AvailableBytes)}, {Key: "swap_total_bytes", Value: optionalInt(v.Sample.SwapTotalBytes)}, {Key: "swap_free_bytes", Value: optionalInt(v.Sample.SwapFreeBytes)}, {Key: "swap_used_percent", Value: used}, {Key: "pressure_some_avg60", Value: avg60}, {Key: "pressure_some_avg10", Value: avg10}}
	return contract.OrderedObject{{Key: "state", Value: v.State}, {Key: "limits_from", Value: v.LimitsFrom}, {Key: "limits", Value: limits}, {Key: "measured", Value: measured},
		{Key: "exceeded", Value: listOf(v.Exceeded)}, {Key: "unmeasured", Value: listOf(v.Unmeasured)}, {Key: "unread", Value: listOf(v.Sample.Unread)}}
}

// HostMemoryFromEnvironment is the bound of a command: the limits from the environment (the defaults where a variable is unset or empty) and a sample taken now from the proc root. A threshold that
// is not a number in its range is an error naming its variable.
func HostMemoryFromEnvironment(getenv func(string) string) (*HostMemoryBound, error) {
	limits, from := DefaultHostMemoryLimits(), LimitsDefault
	for _, t := range []struct {
		name string
		max  float64
		set  func(float64)
	}{
		{EnvHostMinAvailableGiB, 1 << 20, func(n float64) { limits.MinAvailableBytes = int64(math.Round(n * gib)) }},
		{EnvHostMaxSwapPercent, 100, func(n float64) { limits.MaxSwapPercent = n }},
		{EnvHostMaxPressure, 100, func(n float64) { limits.MaxPressureSomeAvg60 = n }},
	} {
		value := strings.TrimSpace(getenv(t.name))
		if value == "" {
			continue
		}
		n, err := strconv.ParseFloat(value, 64)
		if err != nil || math.IsNaN(n) || math.IsInf(n, 0) || n < 0 || n > t.max {
			return nil, fmt.Errorf("%s=%q is not a number from 0 to %g", t.name, value, t.max)
		}
		t.set(n)
		from = LimitsEnvironment
	}
	return &HostMemoryBound{Sample: ReadHostMemory(getenv(EnvHostProcRoot)), Limits: limits, LimitsFrom: from}, nil
}

// useHostMemory gives the scheduler of dag-ready and dag-release its bound; a threshold that is not usable is a usage error (exit 4), and no other command reads these variables.
func (s *Scheduler) useHostMemory() error {
	bound, err := HostMemoryFromEnvironment(os.Getenv)
	if err != nil {
		return usage(err.Error())
	}
	s.Host = bound
	return nil
}
