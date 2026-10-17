package worker

// Observation is a worker sample. Only the controller supplies receipt time.
type Observation struct {
	HostID             string  `json:"host_id"`
	Incarnation        string  `json:"incarnation"`
	BootID             string  `json:"boot_id"`
	Sequence           int64   `json:"sequence"`
	EffectiveCPU       int     `json:"effective_cpu"`
	AvailableBytes     int64   `json:"available_bytes"`
	SwapUsedPercent    float64 `json:"swap_used_percent"`
	PressureSomeAvg60  float64 `json:"pressure_some_avg60"`
	PressureFullAvg10  float64 `json:"pressure_full_avg10"`
	Measured           bool    `json:"measured"`
	ToolchainProfile   string  `json:"toolchain_profile"`
	LaunchProfile      string  `json:"launch_profile"`
	CapabilityRevision string  `json:"capability_revision"`
	Supervised         bool    `json:"supervised"`
	QueuedHeavy        int     `json:"queued_heavy"`
}
