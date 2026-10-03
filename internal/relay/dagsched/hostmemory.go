package dagsched

import "github.com/thisisjun786/codex-relay-workflow/internal/contract"

// Stub of the host memory bound (CRW-468), committed with its tests first so that the tests fail on their assertions before the bound exists.

const (
	EnvHostProcRoot        = "CRW_DAG_HOST_PROC_ROOT"
	EnvHostMinAvailableGiB = "CRW_DAG_HOST_MIN_AVAILABLE_GIB"
	EnvHostMaxSwapPercent  = "CRW_DAG_HOST_MAX_SWAP_PERCENT"
	EnvHostMaxPressure     = "CRW_DAG_HOST_MAX_PRESSURE_SOME_AVG10"
)

const (
	HostMemoryWithin     = "within"
	HostMemoryDeferring  = "deferring"
	HostMemoryUnmeasured = "unmeasured"
)

const (
	LimitsDefault     = "default"
	LimitsEnvironment = "environment"
)

type HostMemory struct {
	AvailableBytes, SwapTotalBytes, SwapFreeBytes *int64
	PressureSomeAvg10                             *float64
	Unread                                        []string
}

type HostMemoryLimits struct {
	MinAvailableBytes                    int64
	MaxSwapPercent, MaxPressureSomeAvg10 float64
}

func DefaultHostMemoryLimits() HostMemoryLimits {
	return HostMemoryLimits{MinAvailableBytes: 15 << 30, MaxSwapPercent: 50, MaxPressureSomeAvg10: 10}
}

type HostMemoryBound struct {
	Sample     HostMemory
	Limits     HostMemoryLimits
	LimitsFrom string
}

type HostMemoryVerdict struct {
	State                string
	Limits               HostMemoryLimits
	LimitsFrom           string
	Sample               HostMemory
	Exceeded, Unmeasured []string
}

func ReadHostMemory(root string) HostMemory { return HostMemory{} }

func (v HostMemoryVerdict) SwapUsedPercent() (float64, bool) { return 0, false }

func (b HostMemoryBound) Judge() HostMemoryVerdict {
	return HostMemoryVerdict{State: HostMemoryWithin, Limits: b.Limits, LimitsFrom: b.LimitsFrom, Sample: b.Sample}
}

func (v HostMemoryVerdict) Detail() string { return "" }

func (v HostMemoryVerdict) object() contract.OrderedObject { return nil }

func HostMemoryFromEnvironment(getenv func(string) string) (*HostMemoryBound, error) {
	return &HostMemoryBound{Limits: DefaultHostMemoryLimits(), LimitsFrom: LimitsDefault}, nil
}

func (s *Scheduler) useHostMemory() error { return nil }
