// Memory job requeue from CXC v0.2.40 recall/src/memory-requeue.ts.
package recall

type RequeueCandidate struct {
	Kind   string `json:"kind"`
	JobKey string `json:"jobKey"`
	Cause  string `json:"cause"`
}

type RequeueOptions struct {
	Apply                bool     `json:"apply"`
	IncludeContextWindow bool     `json:"includeContextWindow"`
	Kind                 string   `json:"kind"`
	Limit                *float64 `json:"limit"`
	Retries              *float64 `json:"retries"`
}

type RequeueResult struct {
	State          MemoryStatusState  `json:"state"`
	Detail         string             `json:"detail"`
	StorePath      *string            `json:"storePath"`
	Applied        bool               `json:"applied"`
	Selected       []RequeueCandidate `json:"selected"`
	SkippedByCause CauseCounts        `json:"skippedByCause"`
	Changed        float64            `json:"changed"`
	Retries        float64            `json:"retries"`
}

func TransientCauses() []string { return nil }
func RequeueExhaustedMemoryJobs(home string, options ...RequeueOptions) RequeueResult {
	return RequeueResult{}
}
func FormatRequeue(result RequeueResult) string { return "" }
