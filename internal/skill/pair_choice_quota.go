package skill

import (
	"math"
	"strings"
	"time"
)

type pairQuotaWindow struct {
	Name        string    `json:"name"`
	Utilization *float64  `json:"utilization"`
	ResetAt     time.Time `json:"reset_at"`
}
type pairQuotaSide struct {
	State      string            `json:"state"`
	ObservedAt time.Time         `json:"observed_at"`
	Windows    []pairQuotaWindow `json:"windows"`
}
type pairSnapshot struct {
	Schema string         `json:"schema"`
	Claude *pairQuotaSide `json:"claude"`
	OpenAI *pairQuotaSide `json:"openai"`
}
type pairHeadroom struct {
	Claude float64 `json:"claude"`
	OpenAI float64 `json:"openai"`
}

func (h pairHeadroom) room(p string) float64 {
	if p == "Sonnet" {
		return h.Claude
	}
	return h.OpenAI
}

type pairQuota struct {
	Readable bool          `json:"readable"`
	Reason   string        `json:"reason,omitempty"`
	Values   *pairSnapshot `json:"values"`
	Headroom *pairHeadroom `json:"headroom"`
}

func readPairQuota(path string, now time.Time) pairQuota {
	q := pairQuota{Reason: "no_snapshot_producer"}
	if path == "" {
		return q
	}
	b, e := pairInput(path, nil)
	if e != nil {
		q.Reason = "snapshot_read_failed"
		return q
	}
	var s *pairSnapshot
	if pairJSON(b, &s) != nil || s == nil || s.Schema != "crw-pair-quota/1" {
		q.Reason = "snapshot_malformed"
		return q
	}
	q.Values = s
	claude, reason := pairSideHeadroom(s.Claude, now)
	if reason != "" {
		q.Reason = "claude: " + reason
		return q
	}
	openai, reason := pairSideHeadroom(s.OpenAI, now)
	if reason != "" {
		q.Reason = "openai: " + reason
		return q
	}
	q.Readable = true
	q.Reason = ""
	q.Headroom = &pairHeadroom{claude, openai}
	return q
}
func pairSideHeadroom(s *pairQuotaSide, now time.Time) (float64, string) {
	if s == nil {
		return 0, "missing_side"
	}
	if s.State == "unknown" {
		return 0, "unknown_side"
	}
	if s.State != "available" && s.State != "exhausted" {
		return 0, "invalid_state"
	}
	if s.ObservedAt.IsZero() || s.ObservedAt.After(now) || now.Sub(s.ObservedAt) > pairQuotaMaxAge {
		return 0, "stale_or_future_observation"
	}
	names := map[string]bool{}
	room := 100.0
	active := false
	for _, w := range s.Windows {
		if strings.TrimSpace(w.Name) == "" || names[w.Name] || w.Utilization == nil || math.IsNaN(*w.Utilization) || *w.Utilization < 0 || *w.Utilization > 100 || w.ResetAt.IsZero() {
			return 0, "invalid_window"
		}
		names[w.Name] = true
		if w.ResetAt.After(now) {
			active = true
			room = math.Min(room, 100-*w.Utilization)
		}
	}
	if !active {
		return 0, "no_active_window"
	}
	if (s.State == "exhausted") != (room == 0) {
		return 0, "inconsistent_state"
	}
	return room, ""
}
