package skill

import (
	"errors"
	"math/big"
	"strconv"
	"strings"
	"time"
)

const pairDecimalLimit = 128

// Keep the input decimal for decisions and JSON output, rather than a binary64 approximation.
type pairPercent string

func (p *pairPercent) UnmarshalJSON(b []byte) error {
	s := string(b)
	if len(s) == 0 || len(s) > pairDecimalLimit || s[0] == '"' {
		return errors.New("invalid quota decimal")
	}
	if at := strings.IndexAny(s, "eE"); at >= 0 {
		e, err := strconv.Atoi(s[at+1:])
		if err != nil || e < -pairDecimalLimit || e > pairDecimalLimit {
			return errors.New("quota exponent out of bounds")
		}
	}
	n, ok := new(big.Rat).SetString(s)
	if !ok || n.Sign() < 0 || n.Cmp(big.NewRat(100, 1)) > 0 {
		return errors.New("quota percent out of range")
	}
	*p = pairPercent(s)
	return nil
}
func (p pairPercent) MarshalJSON() ([]byte, error) { return []byte(p), nil }
func (p pairPercent) rational() *big.Rat           { n, _ := new(big.Rat).SetString(string(p)); return n }

type pairQuotaWindow struct {
	Name        string       `json:"name"`
	Utilization *pairPercent `json:"utilization"`
	ResetAt     time.Time    `json:"reset_at"`
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
	used     [2]*big.Rat
}

func (q pairQuota) shouldSwitch(from string) bool {
	a, b := q.used[0], q.used[1]
	if from == "SOL" {
		a, b = b, a
	}
	return new(big.Rat).Sub(a, b).Cmp(big.NewRat(pairHysteresisPoints, 1)) > 0
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
	claude, reason := pairSideUtilization(s.Claude, now)
	if reason != "" {
		q.Reason = "claude: " + reason
		return q
	}
	openai, reason := pairSideUtilization(s.OpenAI, now)
	if reason != "" {
		q.Reason = "openai: " + reason
		return q
	}
	room := func(used *big.Rat) float64 { v, _ := new(big.Rat).Sub(big.NewRat(100, 1), used).Float64(); return v }
	q.Readable = true
	q.Reason = ""
	q.Headroom = &pairHeadroom{room(claude), room(openai)}
	q.used = [2]*big.Rat{claude, openai}
	return q
}
func pairSideUtilization(s *pairQuotaSide, now time.Time) (*big.Rat, string) {
	if s == nil {
		return nil, "missing_side"
	}
	if s.State == "unknown" {
		return nil, "unknown_side"
	}
	if s.State != "available" && s.State != "exhausted" {
		return nil, "invalid_state"
	}
	if s.ObservedAt.IsZero() || s.ObservedAt.After(now) || now.Sub(s.ObservedAt) > pairQuotaMaxAge {
		return nil, "stale_or_future_observation"
	}
	names := map[string]bool{}
	var used *big.Rat
	for _, w := range s.Windows {
		if strings.TrimSpace(w.Name) == "" || names[w.Name] || w.Utilization == nil || w.ResetAt.IsZero() {
			return nil, "invalid_window"
		}
		names[w.Name] = true
		if w.ResetAt.After(now) {
			n := w.Utilization.rational()
			if used == nil || n.Cmp(used) > 0 {
				used = n
			}
		}
	}
	if used == nil {
		return nil, "no_active_window"
	}
	if (s.State == "exhausted") != (used.Cmp(big.NewRat(100, 1)) == 0) {
		return nil, "inconsistent_state"
	}
	return used, ""
}
